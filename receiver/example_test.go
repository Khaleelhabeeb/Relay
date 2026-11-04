package receiver_test

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/khaleelhabeeb/relay/receiver"
)

func ExampleHandler() {
	secret := os.Getenv("RELAY_SIGNING_SECRET")
	http.Handle("/webhooks/relay", receiver.Handler(secret, func(ctx context.Context, d *receiver.Delivery) error {
		// Delivery is at least once: skip events already processed, and
		// record d.EventID in the same transaction as the business effect.
		log.Printf("event %s from %s: %d bytes", d.EventID, d.Provider, len(d.Body))
		return nil // returning an error makes Relay retry
	}))
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func ExampleVerify() {
	var r *http.Request // an incoming delivery
	d, err := receiver.VerifyRequest(r, []string{os.Getenv("RELAY_SIGNING_SECRET")}, receiver.Options{})
	if err != nil {
		log.Print("rejecting delivery: ", err)
		return
	}
	if env, err := d.Envelope(); err == nil { // payload_format: envelope
		log.Print(env.Type, env.Application)
	}
}
