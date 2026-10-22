package zotero

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseKeyList(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"  ", nil, false},
		{"ABCD2345", []string{"ABCD2345"}, false},
		{"ABCD2345,EFGH6789", []string{"ABCD2345", "EFGH6789"}, false},
		{" ABCD2345 , EFGH6789 ", []string{"ABCD2345", "EFGH6789"}, false},
		{"ABCD2345,", []string{"ABCD2345"}, false},
		{",ABCD2345", []string{"ABCD2345"}, false},
		{"ABCD2345,,EFGH6789", []string{"ABCD2345", "EFGH6789"}, false},
		{"ABCD2345,bogus,EFGH6789", []string{"ABCD2345", "EFGH6789"}, false},
		{"abcd2345", nil, true},
		{"ABCD2345;EFGH6789", nil, true},
	}
	for _, tc := range tests {
		got, err := ParseKeyList(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseKeyList(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseKeyList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseKeyListLimit(t *testing.T) {
	keys := make([]string, MaxKeyList+1)
	for i := range keys {
		keys[i] = NewKey()
	}
	if _, err := ParseKeyList(strings.Join(keys[:MaxKeyList], ",")); err != nil {
		t.Errorf("exactly the limit: %v", err)
	}
	_, err := ParseKeyList(strings.Join(keys, ","))
	if !errors.Is(err, ErrTooManyKeys) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := ParseKeyListMax(strings.Join(keys[:51], ","), 50); !errors.Is(err, ErrTooManyKeys) {
		t.Errorf("custom limit: %v", err)
	}
}

func TestParseTagList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a || b", []string{"a", "b"}},
		{"a||b", []string{"a", "b"}},
		{" a  ||  b c || d ", []string{"a", "b c", "d"}},
		{"a || || b", []string{"a", "b"}},
		{"||", nil},
		{"a | b", []string{"a | b"}},
		{"a|||b", []string{"a", "|b"}},
	}
	for _, tc := range tests {
		if got := ParseTagList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseTagList(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseVersionHeader(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		present bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"0", 0, true, false},
		{"123", 123, true, false},
		{" 7 ", 7, true, false},
		{"-1", 0, true, true},
		{"abc", 0, true, true},
		{"1.5", 0, true, true},
		{"99999999999999999999", 0, true, true},
	}
	for _, tc := range tests {
		got, present, err := ParseVersionHeader(tc.in)
		if got != tc.want || present != tc.present || (err != nil) != tc.wantErr {
			t.Errorf("ParseVersionHeader(%q) = %d, %v, %v", tc.in, got, present, err)
		}
	}
}

func FuzzParseKeyList(f *testing.F) {
	for _, s := range []string{"", "ABCD2345", "ABCD2345,EFGH6789", ",", "a,b", " ABCD2345 ,"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		keys, err := ParseKeyList(s)
		if err != nil {
			if keys != nil {
				t.Fatal("keys returned with error")
			}
			return
		}
		if len(keys) > MaxKeyList {
			t.Fatalf("%d keys", len(keys))
		}
		for _, k := range keys {
			if !ValidKey(k) {
				t.Fatalf("invalid key %q accepted", k)
			}
		}
	})
}

func FuzzParseTagList(f *testing.F) {
	for _, s := range []string{"", "a || b", "a||b", "||", "a|||b", " x "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tags := ParseTagList(s)
		for _, tag := range tags {
			if tag == "" || strings.Contains(tag, "||") || tag != strings.TrimSpace(tag) {
				t.Fatalf("bad tag %q from %q", tag, s)
			}
			if utf8.ValidString(s) && !utf8.ValidString(tag) {
				t.Fatalf("broke UTF-8: %q", tag)
			}
		}
	})
}
