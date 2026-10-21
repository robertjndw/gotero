package zotero

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewKey(t *testing.T) {
	re := regexp.MustCompile(`^[23456789ABCDEFGHIJKLMNPQRSTUVWXYZ]{8}$`)
	seen := map[string]bool{}
	for range 2000 {
		k := NewKey()
		if !re.MatchString(k) || !ValidKey(k) {
			t.Fatalf("bad key %q", k)
		}
		seen[k] = true
	}
	if len(seen) < 1990 {
		t.Errorf("suspiciously many collisions: %d unique of 2000", len(seen))
	}
}

func TestKeyDistribution(t *testing.T) {
	// Rejection sampling should hit every alphabet character.
	counts := map[rune]int{}
	for range 3000 {
		for _, c := range NewKey() {
			counts[c]++
		}
	}
	if len(counts) != len(KeyAlphabet) {
		t.Errorf("saw %d distinct characters, want %d", len(counts), len(KeyAlphabet))
	}
}

func TestValidKey(t *testing.T) {
	tests := map[string]bool{
		"ABCD2345":  true,
		"23456789":  true,
		"abcd2345":  false,
		"ABCD234":   false,
		"ABCD23456": false,
		"ABCD2O45":  false,
		"ABCD2045":  false,
		"ABCD2145":  false,
		"":          false,
		"ABCD 345":  false,
		"ÄBCD2345":  false,
	}
	for in, want := range tests {
		if got := ValidKey(in); got != want {
			t.Errorf("ValidKey(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewAPIKey(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9]{24}$`)
	a, b := NewAPIKey(), NewAPIKey()
	if !re.MatchString(a) || !re.MatchString(b) || a == b {
		t.Errorf("bad keys %q %q", a, b)
	}
}

func TestNewToken(t *testing.T) {
	for _, n := range []int{0, 1, 7, 32, 33} {
		tok := NewToken(n)
		if len(tok) != max(n, 0) {
			t.Errorf("NewToken(%d) length %d", n, len(tok))
		}
		if strings.Trim(tok, "0123456789abcdef") != "" {
			t.Errorf("non-hex token %q", tok)
		}
	}
	if NewToken(-1) != "" {
		t.Error("negative n should give empty token")
	}
}

func TestObjectType(t *testing.T) {
	tests := []struct {
		t      ObjectType
		plural string
		param  string
	}{
		{TypeCollection, "collections", "collectionKey"},
		{TypeSearch, "searches", "searchKey"},
		{TypeItem, "items", "itemKey"},
		{TypeSetting, "settings", "settingKey"},
		{TypeTag, "tags", "tag"},
	}
	for _, tc := range tests {
		if !tc.t.Valid() {
			t.Errorf("%s not valid", tc.t)
		}
		if tc.t.Plural() != tc.plural {
			t.Errorf("%s plural %q", tc.t, tc.t.Plural())
		}
		if tc.t.KeyParam() != tc.param {
			t.Errorf("%s param %q", tc.t, tc.t.KeyParam())
		}
		if got, ok := ParseObjectType(tc.plural); !ok || got != tc.t {
			t.Errorf("ParseObjectType(%q) = %v, %v", tc.plural, got, ok)
		}
	}
	if ObjectType("nope").Valid() {
		t.Error("nope should be invalid")
	}
	if _, ok := ParseObjectType("nopes"); ok {
		t.Error("nopes should not parse")
	}
}
