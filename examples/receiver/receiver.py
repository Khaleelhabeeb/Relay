"""Minimal Relay recipient: verifies X-Relay-Signature and deduplicates on
X-Relay-Event-ID. Standard library only; for demos and integration tests.

Env: PORT (default 8000), RELAY_SIGNING_SECRET (required), NAME (log label).
"""
import hashlib
import hmac
import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SECRET = os.environ["RELAY_SIGNING_SECRET"].encode()
NAME = os.environ.get("NAME", "receiver")
TOLERANCE = 300
seen = set()  # use a durable store in real applications


def verify(headers, body):
    ts = headers.get("X-Relay-Timestamp", "")
    event_id = headers.get("X-Relay-Event-ID", "")
    delivery_id = headers.get("X-Relay-Delivery-ID", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > TOLERANCE:
        return False
    signed = f"v1:{ts}:{event_id}:{delivery_id}:".encode() + body
    expected = hmac.new(SECRET, signed, hashlib.sha256).hexdigest()
    for part in headers.get("X-Relay-Signature", "").split(","):
        part = part.strip()
        if part.startswith("v1=") and hmac.compare_digest(part[3:], expected):
            return True
    return False


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if not verify(self.headers, body):
            self.send_response(401)
            self.end_headers()
            return
        event_id = self.headers["X-Relay-Event-ID"]
        duplicate = event_id in seen
        seen.add(event_id)
        event = json.loads(body)
        kind = event.get("type") or event.get("event")
        print(json.dumps({"receiver": NAME, "event_id": event_id, "type": kind,
                          "attempt": self.headers.get("X-Relay-Attempt"), "duplicate": duplicate}), flush=True)
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", "8000"))), Handler).serve_forever()
