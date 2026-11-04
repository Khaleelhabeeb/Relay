package config

import (
	"strings"
	"testing"
)

// FuzzLoad: arbitrary YAML never panics the loader.
func FuzzLoad(f *testing.F) {
	f.Add(base)
	f.Add("applications: {a: null}\nroutes: [{}]")
	f.Add("delivery: {retry_schedule: [1h, -1s]}")
	f.Add("&a [*a]")
	f.Fuzz(func(t *testing.T, yaml string) {
		c, err := Load(strings.NewReader(yaml), testEnv(nil))
		if err == nil && (c == nil || len(c.Applications) == 0) {
			t.Fatal("accepted config without applications")
		}
	})
}
