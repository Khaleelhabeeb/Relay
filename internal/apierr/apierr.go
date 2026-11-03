// Package apierr writes Relay's JSON error bodies:
// {"error":{"code":"...","message":"..."}}.
package apierr

import (
	"encoding/json"
	"net/http"
)

const (
	InvalidSignature     = "invalid_signature"
	Unauthorized         = "unauthorized"
	InvalidPayload       = "invalid_payload"
	ValidationFailed     = "validation_failed"
	NotFound             = "not_found"
	RegistrationConflict = "registration_conflict"
	PayloadTooLarge      = "payload_too_large"
	RateLimited          = "rate_limited"
	StorageUnavailable   = "storage_unavailable"
	Forbidden            = "forbidden"
	ServerBusy           = "server_busy"
)

type body struct {
	Error detail `json:"error"`
}

type detail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Write sends status with a JSON error body.
func Write(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, body{Error: detail{Code: code, Message: message}})
}

// WriteJSON sends status with v encoded as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
