#!/usr/bin/env bash
# Sign a fixture the way the provider would and POST it to Relay.
# Usage: send-fixture.sh [paystack|flutterwave|monnify|stripe] <file.json> [relay-base-url]
# The provider defaults to paystack, so send-fixture.sh <file.json> still works.
set -euo pipefail
provider=paystack
if [[ ${1:-} == paystack || ${1:-} == flutterwave || ${1:-} == monnify || ${1:-} == stripe ]]; then
  provider=$1
  shift
fi
file=${1:?usage: send-fixture.sh [paystack|flutterwave|monnify|stripe] <file.json> [relay-base-url]}
base=${2:-http://127.0.0.1:8080}
url="$base/webhooks/$provider"
case $provider in
  paystack)
    : "${PAYSTACK_SECRET_KEY:?set PAYSTACK_SECRET_KEY (test key)}"
    sig=$(openssl dgst -sha512 -hmac "$PAYSTACK_SECRET_KEY" -hex < "$file" | awk '{print $NF}')
    header="x-paystack-signature: $sig"
    ;;
  flutterwave)
    : "${FLUTTERWAVE_SECRET_HASH:?set FLUTTERWAVE_SECRET_HASH}"
    sig=$(openssl dgst -sha256 -hmac "$FLUTTERWAVE_SECRET_HASH" -binary < "$file" | openssl base64 -A)
    header="flutterwave-signature: $sig"
    ;;
  monnify)
    : "${MONNIFY_SECRET_KEY:?set MONNIFY_SECRET_KEY}"
    sig=$(openssl dgst -sha512 -hmac "$MONNIFY_SECRET_KEY" -hex < "$file" | awk '{print $NF}')
    header="monnify-signature: $sig"
    ;;
  stripe)
    : "${STRIPE_WEBHOOK_SECRET:?set STRIPE_WEBHOOK_SECRET (whsec_...)}"
    t=$(date +%s)
    sig=$( { printf '%s.' "$t"; cat "$file"; } | openssl dgst -sha256 -hmac "$STRIPE_WEBHOOK_SECRET" -hex | awk '{print $NF}')
    header="Stripe-Signature: t=$t,v1=$sig"
    ;;
esac
curl -sS -X POST -H "Content-Type: application/json" -H "$header" \
  --data-binary @"$file" -w ' (HTTP %{http_code})\n' "$url"
