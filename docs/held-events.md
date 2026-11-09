# Handling held events and failed deliveries

Relay never guesses who owns a payment. When it can't tell, or can't deliver, it holds the event
and tells you. This page says what each situation means and what to do about it. Every action
works three ways, with the same rules and audit trail:

| | CLI | Admin API (`/v1/admin`) | Go (`*relay.Relay`) |
|---|---|---|---|
| What needs attention | `relay stats` | `GET /stats` | `Stats` |
| List held events | `relay events list --held` | `GET /events?held=true` | `Events(admin.EventFilter{Held: true})` |
| Full history of one | `relay events inspect evt_…` | `GET /events/{id}` | `Event` |
| Route again with current config | `relay events reprocess` | `POST /events/reprocess` | `Reprocess` |
| Assign an owner | `relay events reroute evt_… --application app --reason …` | `POST /events/{id}/reroute` | `Reroute` |
| Close without an owner | `relay events dismiss evt_… --reason …` | `POST /events/{id}/dismiss` | `Dismiss` |
| Retry the provider API check | `relay events confirm evt_…` | `POST /events/{id}/confirm` | `RetryConfirmation` |
| List missed deliveries | `relay deliveries list --status exhausted` | `GET /deliveries?status=exhausted` | `Deliveries` |
| Replay one | `relay deliveries retry del_…` | `POST /deliveries/{id}/retry` | `RetryDelivery` |

## Get told, don't poll

Turn on [alerts](../README.md#alerts) so each of these reaches you when it happens, with a reminder
while anything is left:

```yaml
alerts:
  url_env: RELAY_ALERTS_URL   # Slack incoming webhook, or your endpoint with format: json
  format: slack
```

If you run Prometheus, [`examples/prometheus/relay-rules.yml`](../examples/prometheus/relay-rules.yml)
has matching alert rules.

## `event.unresolved`: no application owns a payment

Start with `relay events inspect evt_…`. The routing decision's reason says what Relay looked at,
and the reference shows which application it was probably meant for.

| Cause | Fix |
|---|---|
| The application didn't tag the payment (no prefix, no metadata) | Fix the application. For this event, `reroute` it. |
| A new reference format or a new application | Add a route or `reference_prefixes`, restart, then `reprocess`. |
| The application registers references, but registered this one late | Nothing: a registration within 72 hours routes it automatically. Otherwise `reprocess`. |
| It isn't yours: a test payment, a payment link, another business on the account | `dismiss` it with a reason. |

## `event.ambiguous`: more than one application claims it

Two sources of evidence disagree, for example a route and a registration. The decision's
evidence lists every claimant. Fix the configuration or registration that's wrong so it can't
happen again, then `reroute` the event to the right owner. Relay never changes the owner of an
event that is already matched.

## `delivery.exhausted`: an application missed an event

The application was down or answered with an error for the whole retry window (24 hours by
default). `relay deliveries list --status exhausted --application app_b` shows them with the last
error. Once the application is healthy, replay them all:

```bash
relay deliveries list --status exhausted --application app_b --json | jq -r '.data[].id' | xargs relay deliveries retry
```

The application must accept a replay of an event it may already have processed. It deduplicates
on the event ID.

## `confirmation.failed`: the provider's API never answered

With `confirm_with_api`, Relay checks each routed event against the provider's API before
delivering it. If the API was down for the whole retry window, the event is stored but not
delivered. Once the provider is healthy, `relay events confirm evt_…` checks again and delivers it.

## `confirmation.rejected` and `dedup.conflict`: possible forgery

A **rejected** event carried a valid signature, but the provider's API has no matching
transaction. A **conflicting duplicate** reused a real event's ID with different ownership
identifiers. Either way, someone holding your webhook secret may be forging events. Relay never
delivers them.

A webhook the provider's API disowns while it arrives is refused with `422` and never stored, so
it raises no alert. It shows up as `relay_webhooks_received_total{result="not_confirmed"}` and in
the logs; the [Prometheus rules](../examples/prometheus/relay-rules.yml) cover it.

1. Inspect the event: when it arrived, from where, and what it claimed.
2. Rotate the provider secret, and find out who had it. Every application sharing the account
   may hold it, which is why `confirm_with_api` matters when several applications share one.
3. Check the applications for anything they did based on the real event.

## `event.expired`: nobody resolved it in time

With `retention.held_days` set, held events older than that are dismissed automatically, with a
final alert. It's still in the database, with its history. If it turns out to be someone's
payment, `reroute` it: that works on dismissed events and clears the dismissal, as long as its
payload hasn't been cleared yet (`retention.payload_days` after dismissal).

## Building it into your own admin panel

The admin API is enough to build a "payments needing attention" screen into a tool you already
have:

1. `GET /v1/admin/stats` for a badge count: `events.unresolved + events.ambiguous`.
2. `GET /v1/admin/events?held=true` for the list, paging with `after`.
3. `GET /v1/admin/events/{id}` for the detail view. The routing decisions explain why.
4. Buttons calling `reroute` (with the application and a reason) and `dismiss` (with a reason).
5. Optionally, receive the JSON alert digest at an endpoint of yours to refresh the screen or
   notify someone.

Keep the admin token on the server side of that tool. It can read payment payloads and reroute
any event.
