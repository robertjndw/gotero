package zapi

import (
	"encoding/json"
	"testing"
)

// A bad entry fails on its own and parsing carries on with the rest. The
// HTTP layer only hands over syntactically valid JSON, so the undecodable
// object case can only be reached from here.
func TestParseEntriesContinuesAfterBadEntry(t *testing.T) {
	for name, bad := range map[string]string{
		"not an object":       `5`,
		"undecodable object":  `{"a":`,
		"invalid key":         `{"key":"nope"}`,
		"invalid version":     `{"version":-1}`,
		"non-string key type": `{"key":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			raws := []json.RawMessage{json.RawMessage(bad), json.RawMessage(`{"itemType":"book"}`)}
			out := parseEntries(raws, planOpts{NewKey: func() string { return "AAAAAAA2" }})
			if len(out) != 2 {
				t.Fatalf("got %d entries", len(out))
			}
			if out[0].fail == nil {
				t.Errorf("bad entry did not fail: %+v", out[0])
			}
			if out[1].fail != nil || out[1].key != "AAAAAAA2" || out[1].idx != 1 {
				t.Errorf("entry after the bad one was not parsed: %+v", out[1])
			}
		})
	}

	t.Run("duplicate key", func(t *testing.T) {
		raws := []json.RawMessage{
			json.RawMessage(`{"key":"BBBBBBB2"}`), json.RawMessage(`{"key":"BBBBBBB2"}`), json.RawMessage(`{"key":"CCCCCCC2"}`),
		}
		out := parseEntries(raws, planOpts{})
		if out[1].fail == nil {
			t.Errorf("duplicate did not fail: %+v", out[1])
		}
		if out[2].fail != nil || out[2].key != "CCCCCCC2" {
			t.Errorf("entry after the duplicate was not parsed: %+v", out[2])
		}
	})
}
