// Package routing decides which application owns an event. It is pure: all
// state it needs (static rules, registration history) is passed in.
//
// An event is delivered only when every piece of evidence points to exactly
// one application. No evidence → unresolved; conflicting evidence →
// ambiguous. Neither is ever broadcast.
package routing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/internal/config"
	"github.com/khaleelhabeeb/relay/internal/paystack"
)

type Outcome string

const (
	Matched     Outcome = "matched"
	Unresolved  Outcome = "unresolved"
	Ambiguous   Outcome = "ambiguous"
	Unsupported Outcome = "unsupported"
)

// Registration identifier types.
const (
	IdentifierReference        = "transaction_reference"
	IdentifierDedicatedAccount = "dedicated_account"
)

// Evidence sources.
const (
	SourceRule         = "rule"
	SourceRegistration = "registration"
)

// supportedEvents are the event types v1 routes. Others are stored as
// unsupported until their ownership fields are verified against real payloads.
var supportedEvents = map[string]bool{
	"charge.success": true,
}

// Rule is a static route from configuration.
type Rule struct {
	Name            string // e.g. "routes[0]"
	Application     string
	ReferencePrefix string
	Metadata        map[string]string
}

// Candidate is one piece of ownership evidence.
type Candidate struct {
	Application string `json:"application"`
	Source      string `json:"source"`
	Detail      string `json:"detail"`
}

type Decision struct {
	Outcome     Outcome
	Application string // set when Outcome == Matched
	Candidates  []Candidate
	Reason      string
}

// Registration is one ownership claim on an identifier, active or not.
type Registration struct {
	ID            string
	Application   string
	Active        bool
	CreatedAt     time.Time
	DeactivatedAt time.Time // zero while active
}

// RegistrationLookup returns the full registration history (active and
// inactive) for one identifier.
type RegistrationLookup interface {
	Registrations(ctx context.Context, identifierType, value string) ([]Registration, error)
}

type Engine struct {
	rules []Rule
}

func NewEngine(rules []Rule) *Engine { return &Engine{rules: rules} }

// RulesFromConfig converts configured routes to rules, preserving order.
func RulesFromConfig(routes []config.RouteConfig) []Rule {
	rules := make([]Rule, len(routes))
	for i, r := range routes {
		rules[i] = Rule{
			Name:            fmt.Sprintf("routes[%d]", i),
			Application:     r.Application,
			ReferencePrefix: r.Match.ReferencePrefix,
			Metadata:        r.Match.Metadata,
		}
	}
	return rules
}

// Resolve gathers every rule and registration that claims the event and
// reduces them to a decision. lookup may be nil (no registrations).
func (e *Engine) Resolve(ctx context.Context, eventType string, ids paystack.Identifiers, receivedAt time.Time, lookup RegistrationLookup) (Decision, error) {
	if !supportedEvents[eventType] {
		return Decision{Outcome: Unsupported, Reason: fmt.Sprintf("event type %q is not routed in this version", eventType)}, nil
	}

	var cands []Candidate
	for _, r := range e.rules {
		if detail, ok := r.match(ids); ok {
			cands = append(cands, Candidate{Application: r.Application, Source: SourceRule, Detail: r.Name + " " + detail})
		}
	}
	if lookup != nil {
		claims := []struct{ typ, value string }{{IdentifierReference, ids.Reference}}
		if ids.DedicatedAccount != nil {
			claims = append(claims, struct{ typ, value string }{IdentifierDedicatedAccount, ids.DedicatedAccount.Key()})
		}
		for _, c := range claims {
			if c.value == "" {
				continue
			}
			regs, err := lookup.Registrations(ctx, c.typ, c.value)
			if err != nil {
				return Decision{}, fmt.Errorf("routing: lookup %s: %w", c.typ, err)
			}
			for _, reg := range OwnersAt(regs, receivedAt) {
				cands = append(cands, Candidate{Application: reg.Application, Source: SourceRegistration,
					Detail: fmt.Sprintf("%s %s", c.typ, reg.ID)})
			}
		}
	}

	apps := distinctApps(cands)
	switch len(apps) {
	case 1:
		return Decision{Outcome: Matched, Application: apps[0], Candidates: cands,
			Reason: fmt.Sprintf("%d matching rule(s)/registration(s), one application", len(cands))}, nil
	case 0:
		return Decision{Outcome: Unresolved, Candidates: cands, Reason: unresolvedReason(ids)}, nil
	default:
		return Decision{Outcome: Ambiguous, Candidates: cands,
			Reason: "conflicting owners: " + strings.Join(apps, ", ")}, nil
	}
}

func (r Rule) match(ids paystack.Identifiers) (string, bool) {
	var parts []string
	if r.ReferencePrefix != "" {
		if ids.Reference == "" || !strings.HasPrefix(ids.Reference, r.ReferencePrefix) {
			return "", false
		}
		parts = append(parts, "reference_prefix="+r.ReferencePrefix)
	}
	keys := make([]string, 0, len(r.Metadata))
	for k := range r.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := ids.Metadata[k]; !ok || v != r.Metadata[k] {
			return "", false
		}
		parts = append(parts, "metadata."+k)
	}
	return strings.Join(parts, " "), len(parts) > 0
}

// OwnersAt returns the registrations that own an identifier for an event
// received at t:
//   - claims whose tenure covers t (created at or before t, not deactivated
//     by t); otherwise
//   - the identifier's only-ever registration, if still active. This covers
//     a webhook that beats its registration, without ever handing an old
//     event to a later owner of a reassigned identifier.
func OwnersAt(regs []Registration, t time.Time) []Registration {
	var owners []Registration
	for _, r := range regs {
		if !r.CreatedAt.After(t) && (r.Active || r.DeactivatedAt.After(t)) {
			owners = append(owners, r)
		}
	}
	if len(owners) == 0 && len(regs) == 1 && regs[0].Active {
		owners = regs
	}
	return owners
}

func distinctApps(cands []Candidate) []string {
	seen := map[string]bool{}
	var apps []string
	for _, c := range cands {
		if !seen[c.Application] {
			seen[c.Application] = true
			apps = append(apps, c.Application)
		}
	}
	sort.Strings(apps)
	return apps
}

func unresolvedReason(ids paystack.Identifiers) string {
	if ids.Channel == paystack.ChannelDedicatedNUBAN && ids.DedicatedAccount == nil {
		return "dedicated_nuban event without receiver account fields"
	}
	if ids.Reference == "" && len(ids.Metadata) == 0 && ids.DedicatedAccount == nil {
		return "event carries no ownership identifiers"
	}
	return "no rule or registration matched"
}
