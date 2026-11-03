#!/usr/bin/env bash
# Sign a Paystack fixture with PAYSTACK_SECRET_KEY and POST it to Relay,
# the way Paystack would. Usage: send-fixture.sh <file.json> [relay-url]
set -euo pipefail
file=${1:?usage: send-fixture.sh <file.json> [relay-url]}
url=${2:-http://127.0.0.1:8080/webhooks/paystack}
: "${PAYSTACK_SECRET_KEY:?set PAYSTACK_SECRET_KEY (test key)}"
sig=$(openssl dgst -sha512 -hmac "$PAYSTACK_SECRET_KEY" -hex < "$file" | awk '{print $NF}')
curl -sS -X POST -H "Content-Type: application/json" -H "x-paystack-signature: $sig" \
  --data-binary @"$file" -w ' (HTTP %{http_code})\n' "$url"
