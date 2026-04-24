package router

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"wipe", "wipe"},
		{"  WIPE  ", "wipe"},
		{"Reset Board", "reset board"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalize(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMatchExact verifies that matching is case-insensitive and exact — a
// transcript must equal one of the configured phrases after normalization.
func TestMatchExact(t *testing.T) {
	routes := []compiledRoute{
		{say: "wipe", target: "chess"},
		{say: "reset board", target: "chess"},
		{say: "hard mode", target: "settings"},
	}

	cases := []struct {
		transcript string
		wantMatch  string
	}{
		{"wipe", "wipe"},
		{"WIPE", "wipe"},
		{" wipe ", "wipe"},
		{"Reset Board", "reset board"},
		{"reset", ""},          // partial — should NOT match (exact, not substring)
		{"please wipe", ""},    // extra words — should NOT match
		{"hard mode", "hard mode"},
		{"", ""},
	}

	for _, c := range cases {
		found := ""
		norm := normalize(c.transcript)
		for _, route := range routes {
			if norm == route.say {
				found = route.say
				break
			}
		}
		if found != c.wantMatch {
			t.Errorf("match(%q) = %q, want %q", c.transcript, found, c.wantMatch)
		}
	}
}
