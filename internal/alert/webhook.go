package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/delivery"
)

// Webhook posts digests to a URL: signed JSON, or a Slack message.
type Webhook struct {
	URL       string
	Format    string // config.AlertFormatJSON or config.AlertFormatSlack
	Secret    string // signs the json format
	Client    *http.Client
	UserAgent string
}

func (w *Webhook) Name() string { return w.Format }

func (w *Webhook) Send(ctx context.Context, d *admin.AlertDigest) error {
	var (
		body []byte
		err  error
	)
	if w.Format == config.AlertFormatSlack {
		body, err = json.Marshal(map[string]string{"text": SlackText(d)})
	} else {
		body, err = json.Marshal(d)
	}
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", w.UserAgent)
	if w.Format != config.AlertFormatSlack {
		// The same scheme as deliveries, with the digest ID as both event and
		// delivery ID, so receiver.Verify checks it.
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set(delivery.HeaderEventID, d.ID)
		req.Header.Set(delivery.HeaderDeliveryID, d.ID)
		req.Header.Set(delivery.HeaderTimestamp, ts)
		req.Header.Set(delivery.HeaderSignature, delivery.Sign([]string{w.Secret}, ts, d.ID, d.ID, body))
	}
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("alert endpoint answered %d", resp.StatusCode)
	}
	return nil
}

var kindLabels = map[string]string{
	admin.AlertUnresolved:           "unresolved",
	admin.AlertAmbiguous:            "ambiguous",
	admin.AlertExpired:              "expired",
	admin.AlertDeliveryExhausted:    "delivery exhausted",
	admin.AlertConfirmationFailed:   "confirmation failed",
	admin.AlertConfirmationRejected: "confirmation rejected (possible forgery)",
	admin.AlertDedupConflict:        "conflicting duplicate (possible forgery)",
}

var kindOrder = []string{admin.AlertConfirmationRejected, admin.AlertDedupConflict, admin.AlertAmbiguous,
	admin.AlertUnresolved, admin.AlertDeliveryExhausted, admin.AlertConfirmationFailed, admin.AlertExpired}

// slackEscape escapes the characters Slack's mrkdwn treats as control.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "'").Replace(s)
}

// SlackText renders a digest as a Slack message.
func SlackText(d *admin.AlertDigest) string {
	var b strings.Builder
	s := d.Stats
	if d.Reminder {
		fmt.Fprintf(&b, "*Relay reminder:* still needs attention")
	} else {
		var parts []string
		total := 0
		for _, k := range kindOrder {
			if n := d.Counts[k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, kindLabels[k]))
				total += n
			}
		}
		fmt.Fprintf(&b, "*Relay: %d new alert(s)* (%s)", total, strings.Join(parts, ", "))
		for _, al := range d.Alerts {
			fmt.Fprintf(&b, "\n• %s `%s`", kindLabels[al.Kind], al.EventID)
			if al.Provider != "" {
				fmt.Fprintf(&b, " %s %s", slackEscape(al.Provider), slackEscape(al.EventType))
			}
			if al.Reference != "" {
				fmt.Fprintf(&b, " ref `%s`", slackEscape(al.Reference))
			}
			if al.ApplicationID != "" {
				fmt.Fprintf(&b, " → %s", slackEscape(al.ApplicationID))
			}
			if al.Detail != "" {
				fmt.Fprintf(&b, ": %s", slackEscape(al.Detail))
			}
		}
		if d.Omitted > 0 {
			fmt.Fprintf(&b, "\n…and %d more", d.Omitted)
		}
	}
	fmt.Fprintf(&b, "\nNow: %d unresolved, %d ambiguous, %d exhausted deliveries, %d failed confirmations",
		s.Events.Unresolved, s.Events.Ambiguous, s.Deliveries.Exhausted, s.Events.ConfirmationFailed)
	if !s.Events.OldestHeldAt.IsZero() {
		fmt.Fprintf(&b, " (oldest held since %s)", s.Events.OldestHeldAt.Format("2006-01-02 15:04 MST"))
	}
	b.WriteString("\nResolve with `relay stats` and `relay events list --held`, or the admin API.")
	return b.String()
}
