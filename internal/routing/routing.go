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

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/provider"
	"github.com/khaleelhabeeb/relay/provider/flutterwave"
	"github.com/khaleelhabeeb/relay/provider/monnify"
	"github.com/khaleelhabeeb/relay/provider/paystack"
	"github.com/khaleelhabeeb/relay/provider/stripe"
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

// Rule is a static route from configuration.
type Rule struct {
	Name            string // e.g. "routes[0]"
	Provider        string // "" matches every provider
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

// RetroactiveGrace bounds how long before its registration an event may
// have arrived and still be claimed by it, when the reference is not in the
// registering application's own namespace (or the identifier is a dedicated
// account). It covers a webhook that beats its registration by moments,
// without letting an application claim another's older unrouted payments.
const RetroactiveGrace = 15 * time.Minute

type Engine struct {
	rules      []Rule
	adapters   map[string]provider.Adapter
	names      []string
	namespaces map[string][]string // application → reference prefixes it owns
}

// NewEngine builds an engine over rules for the given providers. Without
// adapters it knows the built-in providers, without secrets: enough to
// route and re-read stored events, not to verify webhooks.
func NewEngine(rules []Rule, adapters ...provider.Adapter) *Engine {
	if len(adapters) == 0 {
		adapters = []provider.Adapter{paystack.Adapter{}, flutterwave.Adapter{}, monnify.Adapter{}, stripe.Adapter{}}
	}
	e := &Engine{rules: rules, adapters: make(map[string]provider.Adapter, len(adapters))}
	for _, a := range adapters {
		if _, dup := e.adapters[a.Name()]; !dup {
			e.names = append(e.names, a.Name())
		}
		e.adapters[a.Name()] = a
	}
	return e
}

// WithNamespaces sets each application's reference namespace (see
// config.ApplicationConfig.ReferencePrefixes) and returns e.
func (e *Engine) WithNamespaces(ns map[string][]string) *Engine {
	e.namespaces = ns
	return e
}

// NamespacesFromConfig collects the applications' reference_prefixes.
func NamespacesFromConfig(apps map[string]*config.ApplicationConfig) map[string][]string {
	ns := map[string][]string{}
	for id, a := range apps {
		if a != nil && len(a.ReferencePrefixes) > 0 {
			ns[id] = a.ReferencePrefixes
		}
	}
	return ns
}

// InNamespace reports whether reference starts with one of app's prefixes.
func (e *Engine) InNamespace(app, reference string) bool {
	for _, p := range e.namespaces[app] {
		if strings.HasPrefix(reference, p) {
			return true
		}
	}
	return false
}

// HasNamespace reports whether app has declared reference prefixes.
func (e *Engine) HasNamespace(app string) bool { return len(e.namespaces[app]) > 0 }

// ReferenceOwner returns the application other than app that a reference
// of prov belongs to by namespace or by a static reference_prefix route,
// and a description of why; "" if none.
func (e *Engine) ReferenceOwner(prov, reference, app string) (owner, why string) {
	for other, prefixes := range e.namespaces {
		for _, p := range prefixes {
			if other != app && strings.HasPrefix(reference, p) {
				return other, fmt.Sprintf("reference prefix %q belongs to %s", p, other)
			}
		}
	}
	for _, r := range e.rules {
		if r.Application != app && (r.Provider == "" || r.Provider == prov) &&
			r.ReferencePrefix != "" && strings.HasPrefix(reference, r.ReferencePrefix) {
			return r.Application, fmt.Sprintf("%s routes reference prefix %q to %s", r.Name, r.ReferencePrefix, r.Application)
		}
	}
	return "", ""
}

// Adapter returns the adapter for a provider name.
func (e *Engine) Adapter(name string) (provider.Adapter, bool) {
	a, ok := e.adapters[name]
	return a, ok
}

// Providers returns the engine's provider names in registration order.
func (e *Engine) Providers() []string { return append([]string(nil), e.names...) }

// RulesFromConfig converts configured routes to rules, preserving order.
func RulesFromConfig(routes []config.RouteConfig) []Rule {
	rules := make([]Rule, len(routes))
	for i, r := range routes {
		rules[i] = Rule{
			Name:            fmt.Sprintf("routes[%d]", i),
			Provider:        r.Provider,
			Application:     r.Application,
			ReferencePrefix: r.Match.ReferencePrefix,
			Metadata:        r.Match.Metadata,
		}
	}
	return rules
}

// Resolve gathers every rule and registration that claims the event and
// reduces them to a decision. lookup may be nil (no registrations) and must
// cover only prov's registrations.
func (e *Engine) Resolve(ctx context.Context, prov, eventType string, ids provider.Identifiers, receivedAt time.Time, lookup RegistrationLookup) (Decision, error) {
	ad, ok := e.adapters[prov]
	if !ok || !ad.Routes(eventType) {
		return Decision{Outcome: Unsupported, Reason: fmt.Sprintf("event type %q is not routed in this version", eventType)}, nil
	}

	var cands []Candidate
	for _, r := range e.rules {
		if r.Provider != "" && r.Provider != prov {
			continue
		}
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
			mayPredate := func(r Registration) bool {
				return (c.typ == IdentifierReference && e.InNamespace(r.Application, c.value)) ||
					!r.CreatedAt.After(receivedAt.Add(RetroactiveGrace))
			}
			for _, reg := range OwnersAt(regs, receivedAt, mayPredate) {
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
		return Decision{Outcome: Unresolved, Candidates: cands, Reason: unresolvedReason(ad, ids)}, nil
	default:
		return Decision{Outcome: Ambiguous, Candidates: cands,
			Reason: "conflicting owners: " + strings.Join(apps, ", ")}, nil
	}
}

func (r Rule) match(ids provider.Identifiers) (string, bool) {
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
//   - the identifier's only-ever registration, if still active and
//     mayPredate allows it to claim an event from before it was created.
//     This covers a webhook that beats its registration, without ever
//     handing an old event to a later owner of a reassigned identifier.
//
// mayPredate nil allows any such claim.
func OwnersAt(regs []Registration, t time.Time, mayPredate func(Registration) bool) []Registration {
	var owners []Registration
	for _, r := range regs {
		if !r.CreatedAt.After(t) && (r.Active || r.DeactivatedAt.After(t)) {
			owners = append(owners, r)
		}
	}
	if len(owners) == 0 && len(regs) == 1 && regs[0].Active && (mayPredate == nil || mayPredate(regs[0])) {
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

func unresolvedReason(ad provider.Adapter, ids provider.Identifiers) string {
	if h, ok := ad.(provider.Hinter); ok {
		if hint := h.UnresolvedHint(ids); hint != "" {
			return hint
		}
	}
	if ids.Reference == "" && len(ids.Metadata) == 0 && ids.DedicatedAccount == nil {
		return "event carries no ownership identifiers"
	}
	return "no rule or registration matched"
}
