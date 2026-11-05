<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/relay-logo-dark.png">
    <img src="assets/relay-logo.png" alt="Relay" width="320">
  </picture>
</p>

<p align="center">
  <strong>One Paystack, Flutterwave or Monnify account. Many applications. Every webhook delivered to the right one.</strong>
</p>

<p align="center">
  <a href="https://github.com/khaleelhabeeb/relay/actions/workflows/ci.yml"><img src="https://github.com/khaleelhabeeb/relay/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/khaleelhabeeb/relay"><img src="https://goreportcard.com/badge/github.com/khaleelhabeeb/relay" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License"></a>
  <img src="https://img.shields.io/badge/go-1.26%2B-00ADD8.svg" alt="Go 1.26+">
</p>

---

Paystack, Flutterwave and Monnify each send all webhooks for an account to **one URL**. If several of your
applications share that account, such as App A, App B and App C, you need something at that URL
that works out which application each payment belongs to and passes the event on to it.

**Relay** is that piece. It's a small self-hosted Go service. It receives your payment provider's
webhooks,
verifies them, decides which application owns each event, stores it safely and then delivers it
to that application with its own signature. If an application is down, Relay retries for up to
24 hours. If no application, or more than one, can be identified as the owner, Relay holds the
event for you to resolve. It never guesses and never broadcasts payment data.

- **One binary, no infrastructure.** Embedded SQLite. No Redis, no message queue, no dashboard
  to host.
- **Safe by default.** Signatures are verified on the exact bytes the provider sent. Relay answers
  the provider only after the event is committed to disk. Delivery is at least once, and recipients
  deduplicate on a stable event ID.
- **Language-agnostic.** Your applications receive a plain signed HTTP POST, so no SDK is
  needed. Verification code is below for Python, PHP, Node.js and Go.

- **Paystack, Flutterwave and Monnify.** Enable any one, or several side by side in one
  instance. Each provider has its own endpoint: `/webhooks/paystack`, `/webhooks/flutterwave` or
  `/webhooks/monnify`. Most examples below use Paystack. The [Flutterwave](#flutterwave) and
  [Monnify](#monnify) sections cover what's different.

Relay does **not** process payments, hold money or replace checking a transaction with your
payment provider.

## Contents

- [How it works](#how-it-works)
- [Try it in five minutes](#try-it-in-five-minutes)
- [Install](#install)
- [Set up Relay for your applications](#set-up-relay-for-your-applications)
- [Receiving events](#receiving-events)
- [Registering ownership](#registering-ownership)
- [Configuration reference](#configuration-reference)
- [Running Relay in production](#running-relay-in-production)
- [Operating Relay](#operating-relay)
- [Switching your provider to Relay](#switching-your-provider-to-relay)
- [Flutterwave](#flutterwave)
- [Monnify](#monnify)
- [Use Relay as a Go library](#use-relay-as-a-go-library)
- [Performance and reliability](#performance-and-reliability)
- [FAQ](#faq)
- [Development](#development)

## How it works

```
                            ┌──────────────────────── Relay ─────────────────────────┐
 Paystack    ── webhook ──▶ │ verify signature ─▶ find owner ─▶ save to SQLite ─▶ 200 │
 Flutterwave ── webhook ──▶ │                                          │             │
 Monnify     ── webhook ──▶ │                                          │             │
                            │                         delivery workers ◀┘             │
                            └──────────────┬──────────────────┬────────────────────────┘
                                           ▼                  ▼
                                         App A              App B         (signed POSTs,
                                                                          retried on failure)
```

1. The provider posts a webhook to Relay. Paystack posts to
   `https://relay.example.com/webhooks/paystack`, Flutterwave to
   `https://relay.example.com/webhooks/flutterwave`, and Monnify to
   `https://relay.example.com/webhooks/monnify`.
2. Relay checks the signature against the raw request body. For Paystack that's
   `x-paystack-signature`, made with your secret key. For Flutterwave it's
   `flutterwave-signature` or `verif-hash`, made with your secret hash. For Monnify it's
   `monnify-signature`, made with your client secret key. Forged requests are rejected.
3. Relay finds the owner. It looks at the transaction's **reference prefix**, its **metadata**,
   and any **references or virtual accounts your applications registered** through Relay's API.
4. Relay saves the event and the decision in one database transaction, and only then answers
   the provider with `200`.
5. A background worker sends the event to the owning application and retries with backoff until
   it gets a `2xx`.

| What Relay found | Status | What happens |
|---|---|---|
| Exactly one owning application | `matched` | Delivered |
| No owner | `unresolved` | Held. You add a rule, the app registers the reference, or you assign it from the CLI. |
| Conflicting owners | `ambiguous` | Held. You decide from the CLI. |
| An event type Relay doesn't route yet | `unsupported` | Stored for audit, nothing to do |

Relay currently routes successful charges:
- **Paystack:** `charge.success`, both checkout payments and deposits into dedicated virtual
  accounts.
- **Flutterwave:** `charge.completed`.
- **Monnify:** `SUCCESSFUL_TRANSACTION`, both checkout payments and deposits into reserved
  accounts.

Refunds, transfers, disputes and subscriptions are stored but not forwarded yet; see the
[FAQ](#faq). Customer IDs and emails are never used to decide ownership, because
one customer can pay several of your applications.

## Try it in five minutes

This runs Relay with two small demo applications on your machine and sends it a sample Paystack
event. The [Flutterwave](#flutterwave) section shows how to send a Flutterwave sample instead. You need Docker, `openssl` and `curl`.

```bash
git clone https://github.com/khaleelhabeeb/relay.git
cd relay
```

Create the secrets file for the demo:

```bash
cat > examples/.env <<EOF
PAYSTACK_SECRET_KEY=sk_test_demo_only
APP_A_RELAY_SIGNING_SECRET=$(openssl rand -hex 32)
APP_A_RELAY_API_TOKEN=$(openssl rand -hex 32)
APP_B_RELAY_SIGNING_SECRET=$(openssl rand -hex 32)
APP_B_RELAY_API_TOKEN=$(openssl rand -hex 32)
EOF
```

Start Relay and the two demo applications, `app_a` and `app_b`:

```bash
docker compose -f examples/docker-compose.yml --env-file examples/.env up --build
```

In a second terminal, sign a sample payment the way Paystack would and send it. Its reference
starts with `APPA_`, so it belongs to `app_a`:

```bash
PAYSTACK_SECRET_KEY=sk_test_demo_only examples/send-fixture.sh testdata/paystack/charge_success_checkout.json
```

You'll see the `app_a` container log the event with a verified signature. Send
`testdata/paystack/charge_success_metadata_object.json` and `app_b` receives it instead,
because its metadata says `"application": "app_b"`.

## Install

| Method | Command |
|---|---|
| Go toolchain (1.26+) | `go install github.com/khaleelhabeeb/relay/cmd/relay@latest` |
| From source | `git clone https://github.com/khaleelhabeeb/relay.git && cd relay && make build`, which produces `bin/relay` |
| Docker (build locally) | `docker build -t relay .` |
| As a Go library | `go get github.com/khaleelhabeeb/relay@latest`; see [Use Relay as a Go library](#use-relay-as-a-go-library) |
| Release binaries and image | Published to [GitHub Releases](https://github.com/khaleelhabeeb/relay/releases) and `ghcr.io/khaleelhabeeb/relay` for every version tag |

Check the install:

```bash
relay --version
```

## Set up Relay for your applications

### 1. Describe your applications

Create `relay.yaml`. Each application needs a URL that Relay delivers to and its own signing
secret. Secrets are never written in the file. Each `*_env` setting names an environment
variable that holds the value.

```yaml
server:
  listen: ":8080"                # webhooks from your providers (put HTTPS in front of this)
  api_listen: "127.0.0.1:8081"   # registration API and metrics, kept off the internet

storage:
  dsn: "/var/lib/relay/relay.db"

providers:                                    # enable one or more
  paystack:
    secret_env: PAYSTACK_SECRET_KEY           # your Paystack secret key (sk_live_… or sk_test_…)
  flutterwave:
    secret_hash_env: FLUTTERWAVE_SECRET_HASH  # the secret hash from Flutterwave's webhook settings
  monnify:
    secret_key_env: MONNIFY_SECRET_KEY        # your Monnify client secret key

applications:
  app_a:
    endpoint: "https://app-a.yourdomain.com/webhooks/relay"
    signing_secret_env: APP_A_RELAY_SECRET   # Relay signs deliveries to this app with it
  app_b:
    endpoint: "https://app-b.yourdomain.com/webhooks/relay"
    signing_secret_env: APP_B_RELAY_SECRET
    registration_token_env: APP_B_RELAY_TOKEN  # optional: lets this app use the registration API
    payload_format: envelope                  # optional: wrap the provider's event in Relay metadata

routes:
  - application: app_a
    match:
      reference_prefix: "APPA_"
  - application: app_b
    match:
      metadata:
        app: "app_b"
```

Generate a secret for each application. Each must be at least 32 bytes and different from the
others:

```bash
openssl rand -hex 32
```

Then check the file:

```bash
relay config validate -c relay.yaml
```

Validation reports every problem at once: a missing variable, an `http://` endpoint, two routes
that would both claim the same payment, and so on. Every option is listed in the
[configuration reference](#configuration-reference).

### 2. Tag each payment with its owner

When your application starts a transaction, include something Relay can recognise. Pick one
approach per application. The examples here use Paystack. For Flutterwave, the reference is
`tx_ref` and the metadata is `meta`, as shown in [Flutterwave](#flutterwave). For Monnify, they
are `paymentReference` and `metaData`, as shown in [Monnify](#monnify).

**A reference prefix.** Start every reference with the application's prefix:

```bash
curl https://api.paystack.co/transaction/initialize \
  -H "Authorization: Bearer $PAYSTACK_SECRET_KEY" -H "Content-Type: application/json" \
  -d '{"email":"parent@example.com","amount":500000,"reference":"APPA_INV_10293"}'
```

**Metadata.** Add a marker to the transaction's metadata:

```json
{ "email": "buyer@example.com", "amount": 250000, "metadata": { "app": "app_b" } }
```

**Registration.** The application registers the reference, or (Paystack only) a dedicated
virtual account, with Relay's API on the private `api_listen` address. This suits
virtual-account deposits, whose references are generated by Paystack. See [Registering ownership](#registering-ownership).

```bash
curl -X POST http://127.0.0.1:8081/v1/registrations \
  -H "Authorization: Bearer $APP_B_RELAY_TOKEN" \
  -d '{"provider":"paystack","type":"dedicated_account","bank":"Wema Bank","account_number":"9930000902"}'
```

If a webhook arrives before its registration, Relay holds it. It routes the event automatically
as soon as the registration arrives, for events up to 72 hours old.

### 3. Accept Relay's deliveries in each application

Add an endpoint that does three things:

1. **Verifies the signature.** See [Verifying the signature](#verifying-the-signature) for the
   rule and ready-made code.
2. **Ignores repeats.** If you've already processed this `X-Relay-Event-ID`, return `200` and
   stop.
3. **Responds quickly with `2xx`.** Do slow work afterwards.

With `payload_format: original` (the default), the body is exactly what the provider sent. Your
existing provider handling code keeps working, and only the signature check
changes. The `X-Relay-Provider` header says which provider the event came from.

### 4. Run Relay behind HTTPS

```bash
export PAYSTACK_SECRET_KEY=sk_live_...          # if paystack is enabled
export FLUTTERWAVE_SECRET_HASH=...              # if flutterwave is enabled
export MONNIFY_SECRET_KEY=...                   # if monnify is enabled
export APP_A_RELAY_SECRET=...
export APP_B_RELAY_SECRET=...
export APP_B_RELAY_TOKEN=...
relay serve -c relay.yaml
```

Put a reverse proxy (Caddy, nginx, a cloud load balancer) in front of it to terminate TLS, and
expose only the `POST /webhooks/<provider>` endpoints you use to the internet. There's a Docker setup in
[Running Relay in production](#running-relay-in-production).

### 5. Point your provider at Relay

**Paystack:** in the Paystack Dashboard, go to **Settings → API Keys & Webhooks** and set the webhook URL to
your Relay address followed by `/webhooks/paystack`. For example, if Relay is served at
`https://relay.yourdomain.com`, the URL is `https://relay.yourdomain.com/webhooks/paystack`.

**Flutterwave:** in **Settings → Webhooks**, set the URL to
`https://relay.yourdomain.com/webhooks/flutterwave` and the secret hash to the value of
`FLUTTERWAVE_SECRET_HASH`, and turn on retries. See [Flutterwave](#flutterwave).

**Monnify:** in **Developers → Webhook URLs**, set the Transaction Completion URL to
`https://relay.yourdomain.com/webhooks/monnify`. See [Monnify](#monnify).

Try it with your **test** keys first. Make a test payment for each application and confirm it
arrives, and that nothing appears in `relay events list --status unresolved`. Before you switch
live traffic, read [Switching your provider to Relay](#switching-your-provider-to-relay).

## Receiving events

Relay sends a `POST` to the application's `endpoint` for each event it owns.

```http
POST /webhooks/relay HTTP/1.1
Content-Type: application/json
User-Agent: Relay/<version>
X-Relay-Event-ID: evt_01JBQ7Z3K9X2R4T6V8W0Y2A4C6     ← stable for this event; deduplicate on it
X-Relay-Delivery-ID: del_01JBQ7Z3M4ABCDEFGHJKMNPQRS
X-Relay-Timestamp: 1793570000
X-Relay-Attempt: 1
X-Relay-Provider: paystack                     ← or flutterwave, monnify
X-Relay-Signature: v1=fe8e97a7670f414a34ec2115f47bcda1ff597e880bfe6c5edf3b823ff2b12b7f

{"event":"charge.success","data":{ ... exactly as the provider sent it ... }}
```

| | |
|---|---|
| **Body, `original` format** (default) | The exact bytes the provider sent |
| **`X-Relay-Provider`** | `paystack`, `flutterwave` or `monnify`. It isn't signed; the body itself identifies the provider. |
| **Body, `envelope` format** | `{"relay_version":1,"id":"evt_…","type":"charge.success","provider":"paystack","application":"app_b","received_at":"2026-11-02T10:15:42.123Z","payload":{ original event, byte for byte }}` |
| **Success** | Any `2xx` response |
| **Retried** | Timeouts, connection errors, non-`2xx` responses and redirects (never followed). By default 10 attempts over about 24 hours: immediately, then after 30s, 2m, 10m, 30m, 1h, 2h, 4h, 8h and 8h, each ±20%. |
| **After the last attempt** | The delivery is marked `exhausted`. Replay it with `relay deliveries retry`. |
| **Timeout per attempt** | `delivery.timeout_seconds`, 10 by default |

### Verifying the signature

1. Read the **raw** request body. Don't parse and re-serialize it first.
2. Reject the request if `X-Relay-Timestamp` is more than **5 minutes** away from your clock.
3. Build the signed message by concatenating these bytes:
   `v1:` + timestamp + `:` + event ID + `:` + delivery ID + `:` + raw body
4. Compute the HMAC-SHA256 of that message with your application's signing secret, hex-encoded.
5. `X-Relay-Signature` holds one or more comma-separated `v1=<hex>` values. During a secret
   rotation it holds two. Accept the request if **any** of them matches, comparing in constant
   time.

The event ID is part of the signed message. That stops anyone from replaying a captured request
under a different event ID to get past your deduplication.

<details>
<summary><strong>Python (FastAPI)</strong></summary>

```python
import hashlib, hmac, time
from fastapi import FastAPI, Request, HTTPException

SECRET = b"..."  # RELAY signing secret for this app
app = FastAPI()

def relay_verified(headers, body: bytes, tolerance: int = 300) -> bool:
    ts = headers.get("x-relay-timestamp", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > tolerance:
        return False
    msg = f"v1:{ts}:{headers.get('x-relay-event-id', '')}:{headers.get('x-relay-delivery-id', '')}:".encode() + body
    expected = hmac.new(SECRET, msg, hashlib.sha256).hexdigest()
    return any(p.strip().startswith("v1=") and hmac.compare_digest(p.strip()[3:], expected)
               for p in headers.get("x-relay-signature", "").split(","))

@app.post("/webhooks/relay")
async def relay_webhook(request: Request):
    body = await request.body()
    if not relay_verified(request.headers, body):
        raise HTTPException(401)
    event_id = request.headers["x-relay-event-id"]
    # if already_processed(event_id): return {"ok": True}
    # ... verify the transaction with your provider, apply effects, record event_id in the same DB transaction
    return {"ok": True}
```

</details>

<details>
<summary><strong>PHP (Laravel)</strong></summary>

```php
function relay_verified(string $secret, \Illuminate\Http\Request $request, int $tolerance = 300): bool
{
    $ts = (string) $request->header('X-Relay-Timestamp', '');
    if (!ctype_digit($ts) || abs(time() - (int) $ts) > $tolerance) {
        return false;
    }
    $message = 'v1:' . $ts . ':' . $request->header('X-Relay-Event-ID', '') . ':'
        . $request->header('X-Relay-Delivery-ID', '') . ':' . $request->getContent();
    $expected = hash_hmac('sha256', $message, $secret);
    foreach (explode(',', (string) $request->header('X-Relay-Signature', '')) as $part) {
        $part = trim($part);
        if (str_starts_with($part, 'v1=') && hash_equals($expected, substr($part, 3))) {
            return true;
        }
    }
    return false;
}

Route::post('/webhooks/relay', function (\Illuminate\Http\Request $request) {
    abort_unless(relay_verified(config('services.relay.secret'), $request), 401);
    $eventId = $request->header('X-Relay-Event-ID');
    // Deduplicate on $eventId, verify with your provider, then fulfil.
    return response()->noContent();
});
```

Exclude this route from CSRF protection.

</details>

<details>
<summary><strong>Node.js (Next.js route handler)</strong></summary>

```ts
import crypto from "node:crypto";

export function relayVerified(secret: string, headers: Headers, body: Buffer, toleranceSec = 300): boolean {
  const ts = headers.get("x-relay-timestamp") ?? "";
  if (!/^\d+$/.test(ts) || Math.abs(Date.now() / 1000 - Number(ts)) > toleranceSec) return false;
  const expected = crypto
    .createHmac("sha256", secret)
    .update(`v1:${ts}:${headers.get("x-relay-event-id") ?? ""}:${headers.get("x-relay-delivery-id") ?? ""}:`)
    .update(body)
    .digest();
  return (headers.get("x-relay-signature") ?? "").split(",").some((part) => {
    const p = part.trim();
    if (!p.startsWith("v1=")) return false;
    const got = Buffer.from(p.slice(3), "hex");
    return got.length === expected.length && crypto.timingSafeEqual(got, expected);
  });
}

// app/webhooks/relay/route.ts
export async function POST(req: Request) {
  const body = Buffer.from(await req.arrayBuffer());
  if (!relayVerified(process.env.RELAY_SIGNING_SECRET!, req.headers, body)) {
    return new Response("invalid signature", { status: 401 });
  }
  const eventId = req.headers.get("x-relay-event-id")!;
  // Deduplicate on eventId, verify with your provider, then fulfil.
  return new Response(null, { status: 204 });
}
```

</details>

<details>
<summary><strong>Go</strong></summary>

Package [`receiver`](https://pkg.go.dev/github.com/khaleelhabeeb/relay/receiver) does this for you:

```go
http.Handle("/webhooks/relay", receiver.Handler(secret, func(ctx context.Context, d *receiver.Delivery) error {
	// deduplicate on d.EventID, then process d.Body; an error makes Relay retry
	return nil
}))
```

Without the dependency:

```go
func RelayVerified(secret string, h http.Header, body []byte, now time.Time) bool {
	ts := h.Get("X-Relay-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v1:" + ts + ":" + h.Get("X-Relay-Event-ID") + ":" + h.Get("X-Relay-Delivery-ID") + ":"))
	m.Write(body)
	want := m.Sum(nil)
	for _, part := range strings.Split(h.Get("X-Relay-Signature"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "v1="); ok {
			if got, err := hex.DecodeString(v); err == nil && hmac.Equal(got, want) {
				return true
			}
		}
	}
	return false
}
```

</details>

**Test vector.** Check your code against this. Turn off the timestamp check while you do, because
the timestamp is fixed in the past.

| Input | Value |
|---|---|
| secret | `whsec_test_0123456789abcdef0123456789abcdef` |
| timestamp | `1793570000` |
| event ID | `evt_01JABC` |
| delivery ID | `del_01JXYZ` |
| body | `{"event":"charge.success"}` |
| signature | `v1=fe8e97a7670f414a34ec2115f47bcda1ff597e880bfe6c5edf3b823ff2b12b7f` |

### Your application's responsibilities

- **Deduplicate on `X-Relay-Event-ID`.** Delivery is at least once. A request can succeed at your
  end and still be retried, for example if Relay crashes before it records the result. Record the
  event ID in the same database transaction as the business effect. `X-Relay-Delivery-ID` and
  `X-Relay-Attempt` are for diagnostics only.
- **Respond quickly.** Do slow work after you respond.
- **Verify the payment with your provider** before granting value: check amount, currency, status and
  reference. A valid Relay signature proves the request came from Relay, not that the payment
  happened. Every application sharing a Paystack account holds its secret key, and anyone with
  that key can send Relay a correctly signed webhook (the same holds for Monnify's secret key). Relay then delivers it like any other.
  Setting `confirm_with_api` on the provider makes Relay check each payment with the provider's API
  before accepting it, but your own check is still the one that protects you.

## Registering ownership

Static routes (reference prefix or metadata) cover most applications. When neither fits, for
example with dedicated virtual accounts, an application can register identifiers through Relay's
API with its own `registration_token`. The token alone decides which application the request
acts for. The API listens on `server.api_listen` if you set it (recommended), otherwise on the
main port.

| Request | Purpose |
|---|---|
| `POST /v1/registrations` | Claim an identifier |
| `GET /v1/registrations?type=&active=&limit=&after=` | List your own registrations, paginated (`limit` 1–500, follow `next`) |
| `DELETE /v1/registrations/{id}` | Deactivate one of your registrations |

```bash
# A transaction reference, before or after the payment ("provider" is paystack, flutterwave or monnify)
curl -X POST http://127.0.0.1:8081/v1/registrations \
  -H "Authorization: Bearer $APP_B_RELAY_TOKEN" \
  -d '{"provider":"paystack","type":"transaction_reference","value":"APPB_ORDER_1001"}'

# A dedicated virtual account (Paystack, or a Monnify reserved account; bank name and account number from the provider)
curl -X POST http://127.0.0.1:8081/v1/registrations \
  -H "Authorization: Bearer $APP_B_RELAY_TOKEN" \
  -d '{"provider":"paystack","type":"dedicated_account","bank":"Wema Bank","account_number":"9930000902"}'

curl -H "Authorization: Bearer $APP_B_RELAY_TOKEN" "http://127.0.0.1:8081/v1/registrations?type=dedicated_account"
curl -X DELETE -H "Authorization: Bearer $APP_B_RELAY_TOKEN" http://127.0.0.1:8081/v1/registrations/reg_01J...
```

| Response | Meaning |
|---|---|
| `201` | Created. `routed_events` counts earlier unresolved webhooks that this registration just routed to you (see below for which). |
| `200` | You already own it. Registering is idempotent. |
| `409 registration_conflict` | Another application owns it, or it falls under another application's `reference_prefixes` or a static route to another application. |
| `401` / `422` / `429` | Bad token; invalid request (including a provider that isn't enabled, or `dedicated_account` for Flutterwave); or rate limited (10 requests per second per application, bursts of 20) |

Registrations are kept separately per provider. A Paystack registration of a reference never
claims a Flutterwave event with the same reference, and the reverse holds too.

**Who can claim what:**
- **Transaction references** can never move to another application, even after deactivation.
  The original owner can claim them again.
- **Virtual accounts** can move to another application only after the current owner deactivates
  them. Each deposit goes to whoever owned the account when it arrived.
- **A registration never overrides a static route** or another application's namespace. A
  reference that starts with another application's `reference_prefixes`, or with a
  `reference_prefix` routed to another application, is refused with `409`.
- **Reference namespaces.** Give each application `reference_prefixes` so nobody else can register
  its references. An application that has them can only register references inside them, and
  gets `422` for anything else.
- **Webhooks that arrived first.** A registration also routes matching unresolved webhooks that
  arrived before it. Inside the application's own `reference_prefixes` it picks up any from the
  last 72 hours. Anywhere else, including virtual accounts, it only picks up ones that arrived
  at most 15 minutes before the registration. Anything older stays `unresolved` for an
  operator to `reroute`. Without this limit, an application could claim another application's
  unrouted payments just by registering their references.

## Configuration reference

Relay reads one YAML file, set with `--config`. Unknown keys are rejected. Secrets never go in
the file: each `*_env` field names an environment variable. `relay config validate` checks
everything, including that those variables are set.

<details>
<summary><strong>All options</strong></summary>

```yaml
version: 1                      # optional; only 1 is supported

server:
  listen: ":8080"               # webhooks, /healthz, /readyz (and the API/metrics unless api_listen is set)
  api_listen: "127.0.0.1:8081"  # optional private listener for /v1/registrations and /metrics
  public_base_url: "https://relay.example.com"   # informational
  max_body_bytes: 1048576       # inbound webhook cap (default 1 MiB, max 16 MiB)
  max_concurrent_webhooks: 256  # webhooks processed at once; excess get 503 + Retry-After (the provider retries)
  metrics: false                # expose GET /metrics (Prometheus)
  trusted_proxies: []           # IPs/CIDRs of your reverse proxies (needed for allowed_ips behind a proxy)

storage:
  driver: sqlite                # only sqlite
  dsn: "/var/lib/relay/relay.db"  # local disk only; Relay creates the directory (0700) and file (0600)

providers:                      # at least one provider
  paystack:
    secret_env: PAYSTACK_SECRET_KEY   # required: Paystack secret key (verifies x-paystack-signature)
    allowed_ips: []                   # optional source allowlist (IPs/CIDRs); empty = allow all
    confirm_with_api: false           # check each routed event with Paystack's verify API before accepting it
  flutterwave:
    secret_hash_env: FLUTTERWAVE_SECRET_HASH  # required: dashboard secret hash (verif-hash / flutterwave-signature)
    allowed_ips: []                   # optional; Flutterwave's IPs change, so usually leave empty
    confirm_with_api: false           # check each routed event with Flutterwave's verify API before accepting it
    secret_key_env: FLUTTERWAVE_SECRET_KEY    # required with confirm_with_api: Flutterwave secret key (FLWSECK-...)
  monnify:
    secret_key_env: MONNIFY_SECRET_KEY        # required: client secret key (verifies monnify-signature)
    allowed_ips: []                   # optional; Monnify publishes 35.242.133.146
    confirm_with_api: false           # check each routed event with Monnify's transaction query API before accepting it
    api_key_env: MONNIFY_API_KEY      # required with confirm_with_api: Monnify API key (MK_...)
    api_base_url: ""                  # default https://api.monnify.com; https://sandbox.monnify.com for sandbox
    allow_unsigned_sandbox: false     # sandbox only: accept unsigned webhooks the sandbox API confirms (see Monnify)

applications:
  app_b:                         # application ID: lowercase letters, digits, _ or -
    endpoint: "https://app-b.yourdomain.com/webhooks/relay"
    signing_secret_env: APP_B_RELAY_SECRET            # required, ≥32 bytes, unique per app
    previous_signing_secret_env: APP_B_RELAY_SECRET_OLD  # optional, for rotation
    registration_token_env: APP_B_RELAY_TOKEN         # optional, ≥32 bytes, unique; enables the API
    reference_prefixes: ["APPB_"]  # optional: references only this app may register (see Registering ownership)
    payload_format: original    # original (default) | envelope

routes:                         # static ownership rules
  - application: app_b
    provider: paystack          # optional: only events from this provider; omitted = every provider
    match:
      reference_prefix: "APPB_" # reference starts with this (case-sensitive): Paystack data.reference, Flutterwave data.tx_ref, Monnify paymentReference (accountReference for reserved accounts)
      metadata:                 # every key equals its value (Paystack data.metadata; Flutterwave data.meta / meta_data; Monnify metaData)
        app: "app_b"             # both matchers set: both must match

delivery:
  max_attempts: 10              # per cycle; a manual retry starts a new cycle (1–50)
  timeout_seconds: 10           # per attempt (1–120)
  worker_count: 8               # concurrent deliveries (1–256)
  retry_schedule: [0s, 30s, 2m, 10m, 30m, 1h, 2h, 4h, 8h, 8h]  # delay before attempt N; last repeats; ±20% jitter
  allow_insecure_local: false   # DEVELOPMENT ONLY: allow http:// and private/loopback destinations

retention:
  payload_days: 90              # clear raw payloads this long after successful delivery
  attempt_days: 90              # delete attempt history older than this
```

</details>

**Validation rules**
- **Applications:** at least one is required. Signing secrets and registration tokens must each
  be at least 32 bytes and different for every application.
- **Endpoints:** must use `https`, carry no credentials, and must not be `localhost` or a private
  IP. Relay also checks the resolved address every time it connects, which blocks DNS tricks.
  `allow_insecure_local` turns these checks off for local development only.
- **Routes:** two routes to different applications are rejected if one always matches whenever
  the other does. Examples: prefixes `APPA_` and `APPA_ED_`, or identical metadata matches.
  Overlaps that depend on the individual payment are caught at runtime, and the event becomes
  `ambiguous`. Routes scoped to different `provider`s never overlap. A route's `provider` must be
  configured.
- **Reference namespaces:** `reference_prefixes` of different applications must not overlap each
  other, or a `reference_prefix` route to a different application.

<details>
<summary><strong>Rotating an application's signing secret</strong></summary>

1. Set `previous_signing_secret_env` to the current secret and `signing_secret_env` to the new
   one. Restart Relay. Each request now carries two `v1=` signatures.
2. Deploy the new secret to the application. It accepts either signature in the meantime.
3. Remove `previous_signing_secret_env` and restart again.

</details>

<details>
<summary><strong>Restricting webhooks to your provider's IP addresses (optional)</strong></summary>

Signature verification is what actually protects the endpoint. An allowlist adds defence in
depth. Paystack publishes its webhook source IPs: `52.31.139.75`, `52.49.173.169` and
`52.214.14.220`. Check Paystack's documentation for changes before relying on them.

```yaml
providers:
  paystack:
    allowed_ips: ["52.31.139.75", "52.49.173.169", "52.214.14.220"]
server:
  trusted_proxies: ["10.0.0.0/8"]   # the addresses your reverse proxy connects from
```

- **Without `trusted_proxies`,** the client is the direct TCP peer.
- **Behind a trusted proxy,** Relay uses the rightmost `X-Forwarded-For` address that isn't
  itself a trusted proxy. It never trusts entries further left, because clients can forge them.
- **Rejected requests** get `403`.
- **Each provider has its own `allowed_ips`.** Flutterwave says its IP addresses are dynamic, so
  leave `providers.flutterwave.allowed_ips` empty unless Flutterwave gives you a fixed list.
  Monnify publishes one address, `35.242.133.146`, which you can put in
  `providers.monnify.allowed_ips`.

</details>

## Running Relay in production

```bash
docker build -t relay .
docker volume create relay-data
docker run -d --name relay --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  --env-file /etc/relay/relay.env \
  -v /etc/relay/relay.yaml:/etc/relay/relay.yaml:ro \
  -v relay-data:/var/lib/relay \
  relay
```

The image is distroless, runs as a non-root user and keeps its data in the `/var/lib/relay`
volume. Set `storage.dsn: /var/lib/relay/relay.db` in the config you mount.

- **Run one Relay per database.** A lock file prevents a second instance. Keep the database on a
  local disk, not a network share.
- **Expose only the webhook endpoint.** Terminate TLS at a reverse proxy, publish only
  the `POST /webhooks/<provider>` endpoints you use, and keep `api_listen` on a private interface.

### Health checks and metrics

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | The process is serving (liveness) |
| `GET /readyz` | The database answers and accepts a write (readiness). Fails on a full disk or read-only database. |
| `GET /metrics` | Prometheus metrics, with `server.metrics: true`, served on `api_listen` when set |

| Alert on | Because |
|---|---|
| `relay_events_unrouted{status="unresolved"}` or `{status="ambiguous"}` above 0 for 15 minutes | A payment has no owner |
| `relay_deliveries{status="exhausted"}` above 0 | An application missed events and needs a replay |
| `relay_oldest_pending_delivery_age_seconds` above 3600 | An application has been failing for a long time |
| `relay_webhooks_received_total{result="storage_error"}` rising (labelled by `provider`) | Relay can't save events; check the disk |
| `relay_webhooks_received_total{result="invalid_signature"}` rising | Wrong Paystack secret key, Flutterwave secret hash or Monnify secret key (see the `provider` label), or someone probing the endpoint |
| `relay_store_up` equal to 0 | The database can't be read |

Logs are JSON on stderr. They carry `event_id`, `delivery_id` and `application`, and never
contain payment payloads or secrets.

### Backups, upgrades and retention

- **Back up** while Relay runs. The file contains payment events, so encrypt it:
  `sqlite3 /var/lib/relay/relay.db "VACUUM INTO '/backups/relay-$(date +%F).db'"`
- **Restore:** stop Relay, put the backup in place as `relay.db`, delete `relay.db-wal` and
  `relay.db-shm`, then start Relay. To recover events received since the backup, resend them
  from your provider. Duplicates are ignored.
- **Upgrade:** back up, stop the old version and start the new one. Schema migrations run on
  startup. Deliveries that were in flight are resumed first. Use the CLI from the same version as
  the server; it refuses a mismatched database.
- **Retention:** every 6 hours Relay clears raw payloads of delivered and `unsupported` events
  older than `retention.payload_days`, and deletes attempt history older than
  `retention.attempt_days`. Events that are unresolved, ambiguous, pending or exhausted keep
  their payloads until they're resolved.

## Operating Relay

The `relay` CLI works against a running server. It only needs the config file, not the secrets.

| Task | Command |
|---|---|
| See payments nobody claimed | `relay events list --status unresolved` |
| Full history of one event | `relay events inspect evt_…` (add `--show-payload` to print the payment data) |
| Re-run routing after adding a rule | `relay events reprocess` |
| Assign an owner yourself (recorded with a reason) | `relay events reroute evt_… --application app_b --reason "ticket 42"` |
| See deliveries an application missed | `relay deliveries list --status exhausted` |
| Replay them once the application is back | `relay deliveries retry del_…` |
| List registrations | `relay registrations list --application app_b` |

Every list command accepts `--json`. To replay everything an application missed:

```bash
relay deliveries list --status exhausted --application app_b --json | jq -r '.[].id' | xargs relay deliveries retry
```

**When an event is `unresolved`,** run `relay events inspect` to see its reference, metadata and
the reason. Then do one of these:
- add or fix a route, restart, and run `relay events reprocess`;
- have the application register the identifier, which routes the event automatically;
- assign the owner with `relay events reroute`.

**When an event is `ambiguous`,** two sources of evidence disagree. Fix the configuration so it
can't happen again, then decide the owner with `relay events reroute`. Relay never changes the
owner of an event that is already `matched`.

## Switching your provider to Relay

Changing the webhook URL moves **every** webhook for the account at once. Go step by step:

1. **Take inventory.** List every system that handles the provider's webhooks today, and the event
   types each one relies on.
2. **Configure ownership.** Give every application that takes payments a reference prefix, a
   metadata marker or registrations. Applications using dedicated virtual accounts must register
   their accounts.
3. **Update receivers.** Each application verifies Relay's signature and deduplicates on
   `X-Relay-Event-ID`.
4. **Rehearse in test mode.** Point your provider's **test** webhook URL at Relay. Make real test
   payments through each application, and confirm nothing shows up as `unresolved` or
   `ambiguous`.
5. **Plan for other event types.** Refunds, transfers, disputes and subscriptions aren't forwarded
   yet. Make sure nothing depends on them, or keep handling them another way.
6. **Switch the live URL,** then watch the unrouted and exhausted metrics closely for the first
   day.

The steps are the same for every provider. Use Flutterwave's **test mode** webhook settings or
Monnify's sandbox for step 4.

## Flutterwave

Relay handles Flutterwave webhooks the same way it handles Paystack's: verify, find the owner,
store, deliver. Enable it alongside Paystack, or on its own:

```yaml
providers:
  flutterwave:
    secret_hash_env: FLUTTERWAVE_SECRET_HASH

routes:
  - application: app_a
    match:
      reference_prefix: "APPA_"      # matches tx_ref; applies to both providers
  - provider: flutterwave
    application: app_b
    match:
      metadata:
        app: "app_b"                 # a key you set in the payment's meta
```

1. In the Flutterwave dashboard, go to **Settings → Webhooks**. Set the URL to your Relay address
   followed by `/webhooks/flutterwave`, for example `https://relay.yourdomain.com/webhooks/flutterwave`.
2. Set a **secret hash** there. Use a long random value (`openssl rand -hex 32`) and put the same
   value in `FLUTTERWAVE_SECRET_HASH`.
3. Turn on **webhook retries**.
4. Start each payment with a `tx_ref` that carries the application's prefix, or add a marker to
   its `meta`. Applications can also register a `tx_ref` with
   `{"provider":"flutterwave","type":"transaction_reference","value":"..."}`.

| | Flutterwave |
|---|---|
| **Signature** | `flutterwave-signature` (base64 HMAC-SHA256 of the body) or `verif-hash` (the secret hash itself). Either one is accepted when it's valid. |
| **Routed events** | `charge.completed`. Other events, such as `transfer.completed`, are stored as `unsupported` until their shape is checked against real payloads. |
| **Reference** | `data.tx_ref`, or `data.reference` for transfers |
| **Metadata** | `data.meta`, plus the top-level `meta_data`. Where both set a key, `data.meta` wins. |
| **Duplicates** | `<event>:<data.id>:<data.status>`, as Flutterwave recommends. A failed charge followed by a successful one with the same ID counts as two events. Anything without an ID and status falls back to a SHA-256 of the body. |
| **Dedicated accounts** | Not supported. Route virtual-account payments by `tx_ref` registration or metadata instead. |
| **Provider retries** | Only 3, 30 minutes apart. Keep Relay's downtime short, and keep re-querying pending transactions as Flutterwave advises. |

**Tagging a Flutterwave payment** with a reference prefix and a metadata marker:

```bash
curl https://api.flutterwave.com/v3/payments \
  -H "Authorization: Bearer $FLW_SECRET_KEY" -H "Content-Type: application/json" \
  -d '{"tx_ref":"APPA_INV_10293","amount":"5000","currency":"NGN","redirect_url":"https://app-a.yourdomain.com/paid","customer":{"email":"buyer@example.com"},"meta":{"app":"app_a"}}'
```

**Trying it locally:** sign a sample Flutterwave event and send it to a running Relay.

```bash
FLUTTERWAVE_SECRET_HASH=your_hash examples/send-fixture.sh flutterwave testdata/flutterwave/charge_completed_card.json
```

`testdata/flutterwave/MATRIX.md` lists the samples and what each one exercises.

Deliveries carry `X-Relay-Provider: flutterwave`, and envelopes carry `"provider":"flutterwave"`.
As with Paystack, check a transaction with Flutterwave's Verify Transaction API before giving value.

## Monnify

Relay handles Monnify webhooks the same way as the others: verify, find the owner, store,
deliver. Enable it alongside other providers, or on its own:

```yaml
providers:
  monnify:
    secret_key_env: MONNIFY_SECRET_KEY
    confirm_with_api: true                # recommended when several applications share the account
    api_key_env: MONNIFY_API_KEY
    allowed_ips: ["35.242.133.146"]

routes:
  - application: app_a
    match:
      reference_prefix: "APPA_"      # matches paymentReference; applies to every provider
  - provider: monnify
    application: app_b
    match:
      metadata:
        app: "app_b"                 # a key you set in the transaction's metaData
```

1. In the Monnify dashboard, go to **Developers → Webhook URLs**. Set **Transaction Completion**
   to your Relay address followed by `/webhooks/monnify`, for example
   `https://relay.yourdomain.com/webhooks/monnify`.
2. Put the account's **client secret key** (Developers → API Keys & Contracts) in
   `MONNIFY_SECRET_KEY`. Monnify signs webhooks with it, and the same key authenticates the API.
3. Start each payment with a `paymentReference` that carries the application's prefix, or add a
   marker to its `metaData`. Applications can also register a reference with
   `{"provider":"monnify","type":"transaction_reference","value":"..."}`.
4. For **reserved accounts**, either give each account an `accountReference` that carries the
   application's prefix, or register the account once it's created:
   `{"provider":"monnify","type":"dedicated_account","bank":"Wema bank","account_number":"..."}`.

| | Monnify |
|---|---|
| **Signature** | `monnify-signature`: lowercase hex HMAC-SHA512 of the body, keyed with the client secret key. Monnify signs **production** webhooks only; see sandbox below. |
| **Routed events** | `SUCCESSFUL_TRANSACTION`. Other events, such as `REJECTED_PAYMENT`, disbursements, refunds and settlements, are stored as `unsupported` until their shape is checked against real payloads. |
| **Reference** | `eventData.paymentReference`. For reserved-account deposits Monnify generates that value, so Relay uses `product.reference` (your `accountReference`) instead. |
| **Metadata** | `eventData.metaData`, as an object or a JSON-encoded string |
| **Dedicated accounts** | Reserved-account deposits carry `destinationAccountInformation` (bank name and account number), matched against `dedicated_account` registrations. |
| **Duplicates** | `<eventType>:<transactionReference>:<paymentStatus>`. The body has no event ID. Anything without a reference and status falls back to a SHA-256 of the body. |
| **Confirmation** | With `confirm_with_api`, Relay logs in with the API key and secret key (caching the token) and checks `GET /api/v2/merchant/transactions/query`. The references, status, amount paid and currency must match. |
| **Provider retries** | Monnify resends when it gets no `200` or the request times out. It doesn't publish the schedule, so keep Relay's downtime short. |

**Sandbox webhooks are unsigned.** Monnify only sends `monnify-signature` in production, so by
default Relay rejects every sandbox webhook with `401`. To test against the sandbox, opt in:

```yaml
providers:
  monnify:
    secret_key_env: MONNIFY_SECRET_KEY    # sandbox secret key
    api_key_env: MONNIFY_API_KEY          # sandbox API key (MK_TEST_...)
    confirm_with_api: true                # required
    api_base_url: "https://sandbox.monnify.com"   # required
    allow_unsigned_sandbox: true
```

With that set, an unsigned webhook is accepted only if it's a routed event type and Monnify's
sandbox API confirms the transaction exactly. A webhook with a wrong signature is still
rejected. Relay refuses to start if `allow_unsigned_sandbox` points at any API other than
`sandbox.monnify.com`, and it logs a warning at startup whenever the option is on.

**Trying it locally:** sign a sample Monnify event and send it to a running Relay.

```bash
MONNIFY_SECRET_KEY=your_key examples/send-fixture.sh monnify testdata/monnify/successful_transaction_card.json
```

`testdata/monnify/MATRIX.md` lists the samples and what each one exercises.

Deliveries carry `X-Relay-Provider: monnify`, and envelopes carry `"provider":"monnify"`. Check
a transaction with Monnify's transaction query API before giving value.

## Use Relay as a Go library

`relay serve` is a thin wrapper around a Go package, so you can import Relay into your own
program:

```go
cfg, err := config.LoadFile("relay.yaml")              // or build a config.Config in Go
if err != nil { log.Fatal(err) }
r, err := relay.New(ctx, cfg, relay.WithLogger(slog.Default()))
if err != nil { log.Fatal(err) }
defer r.Close()

mux.Handle("/webhooks/", r.WebhookHandler())           // in your existing server
go r.Run(ctx)                                          // delivery and retention workers

// Claim a reference in-process when you create an order:
_, routed, err := r.RegisterReference(ctx, "billing", "paystack", "ORDER_1001")
```

| Package | Use it to |
|---|---|
| `relay` | Embed Relay: `New`, `Handler`/`WebhookHandler`/`APIHandler`, `Run`/`ListenAndServe`, `RegisterReference` |
| `relay/config` | Load `relay.yaml`, or build and `Validate` a configuration in Go |
| `relay/receiver` | Verify Relay's deliveries in a Go application (`receiver.Handler`) |
| `relay/provider` | The `Adapter` interface. Implement it and pass `relay.WithProvider` to add a payment provider. |
| `relay/provider/paystack`, `relay/provider/flutterwave`, `relay/provider/monnify` | The built-in adapters, also usable on their own |

The full guide is in [docs/library.md](docs/library.md): embedding, configuration in Go,
receiving deliveries, and writing a provider adapter. The API reference, with runnable examples,
is on [pkg.go.dev](https://pkg.go.dev/github.com/khaleelhabeeb/relay). To add a provider, implement
[`provider.Adapter`](https://pkg.go.dev/github.com/khaleelhabeeb/relay/provider#Adapter):
verify the exact raw body in constant time, keep parsing free of secrets, return a stable dedup
key, and route only event types you have checked against real payloads. The built-in adapters
in `provider/paystack`, `provider/flutterwave` and `provider/monnify` are complete examples.

The API is pre-1.0 and may change between minor versions; [CHANGELOG.md](CHANGELOG.md) records
every change.

## Performance and reliability

Measured on an Apple M1 laptop with durable commits, sending 3,000 unique signed webhooks:

| Concurrent senders | Webhooks accepted per second | Time to acknowledge (median / 99th percentile) | End-to-end delivered per second |
|---|---|---|---|
| 16 | ~2,480 | 4 ms / 31 ms | ~1,820 |
| 64 | ~3,430 | 13 ms / 80 ms | ~1,545 |

That's far above real payment-provider volumes: a million successful charges a month is about 0.4
webhooks per second. On Linux, commit speed depends on your disk; network-attached cloud volumes
can be several times slower. Run `make bench` on your own hardware.

- **Crash testing:** `make chaos` kills a real Relay process with SIGKILL six times, during ingest
  and during delivery, while provider-like senders retry and a flaky receiver fails 10% of
  requests. Across repeated runs, no acknowledged event was lost, every event was delivered, and
  every signature verified.
- **Fuzzing:** `make fuzz` covers signature verification, webhook parsing, envelope construction
  and config loading. About 14.6 million executions ran with no failures.

## FAQ

**Does a delivery from Relay prove the customer paid?**
It proves the event came from Relay, which verified that it came from your provider.
Before giving value, check the amount, currency and status with the provider's Verify Transaction
API, as you would without Relay.

**Can my application receive the same event twice?**
Yes. Delivery is at least once, so always deduplicate on `X-Relay-Event-ID`. Retries and replays
keep the same ID.

**What happens if Relay is down?**
The provider gets no `200`, so it retries.
- **Paystack** retries every 3 minutes for the first 4 tries, then hourly for 72 hours in live mode.
- **Flutterwave** retries only 3 times, 30 minutes apart, and only if retries are enabled in its
  dashboard.
- **Monnify** resends when it gets no `200` or times out, on a schedule it doesn't publish.

Keep downtime short, especially with Flutterwave. Relay ignores anything that arrives twice.

**What happens if my application is down?**
Relay keeps retrying for about 24 hours by default, then marks the delivery `exhausted` for you
to replay. Tune `delivery.max_attempts` and `delivery.retry_schedule` to change that.

**Can I run several Relay instances for high availability?**
Not on one database in this version. Relay is a single process with a lock file. The providers' own
retries cover restarts and short outages.

**Why aren't refunds, transfers or subscriptions forwarded?**
Their payloads identify the owner differently, and Relay only routes event shapes that have been
checked against real provider payloads. They're stored, so nothing is lost, and support is
planned.

**How are duplicate webhooks detected?**
- **Paystack `charge.success`:** by the transaction ID, so a resent event is recognised even if
  its bytes differ.
- **Flutterwave:** by `event:data.id:data.status`, as Flutterwave recommends.
- **Monnify:** by `eventType:transactionReference:paymentStatus`.
- **Anything else:** by a SHA-256 hash of the exact body.

A genuine resend always carries the same reference and virtual account. If a "duplicate" carries
different ones, it suggests forgery: someone with the provider secret may have sent a fake event
first to take the transaction ID. Relay keeps that body, logs an error and counts
`relay_webhooks_received_total{result="dedup_conflict"}`. `relay events inspect` lists it.

## Development

You need Go 1.26 or later.

```bash
make build    # bin/relay
make test     # unit and integration tests
make race     # tests with the race detector
make fuzz     # fuzz the parsers that handle untrusted input
make chaos    # kill a real relay process mid-flight and check nothing is lost
make bench    # throughput and latency on this machine
```

| Directory | Contents |
|---|---|
| `.` (package `relay`) | The embeddable library; `relay serve` runs it |
| `config` | Configuration loading and validation (public) |
| `provider` | Provider-neutral event model and the `Adapter` interface (public) |
| `provider/paystack`, `provider/flutterwave`, `provider/monnify` | Built-in provider adapters (public) |
| `receiver` | Verifying Relay's deliveries in Go (public) |
| `cmd/relay` | Entry point |
| `internal/routing` | Ownership decisions (pure, no I/O) |
| `internal/ingest` | The webhook endpoint |
| `internal/store` | SQLite storage and migrations |
| `internal/delivery` | Workers, retries, outbound signing, SSRF protection |
| `internal/registration` | The `/v1/registrations` API |
| `internal/cli` | The `relay` command |
| `examples` | Sample config, docker-compose demo, demo receiver |
| `test/chaos` | Crash and load tests against the real binary |

Issues and pull requests are welcome. Please run `make test` and `go vet ./...` before opening a
pull request. Release history is in [CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE)
