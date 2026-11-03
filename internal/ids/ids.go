// Package ids generates prefixed, time-sortable identifiers such as
// "evt_01JBQ7Z3K9X2...".
package ids

import "github.com/oklog/ulid/v2"

const (
	Event           = "evt"
	Delivery        = "del"
	Attempt         = "att"
	Registration    = "reg"
	RoutingDecision = "rtd"
)

// New returns prefix + "_" + a new ULID. Safe for concurrent use.
func New(prefix string) string {
	return prefix + "_" + ulid.Make().String()
}
