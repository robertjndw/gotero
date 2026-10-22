package conformance

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
)

var settingsTests = []testCase{
	{"PostAndGet", testSettingsPostGet},
	{"UnchangedWriteDoesNotBump", testSettingsUnchanged},
	{"ChangedWriteBumps", testSettingsChanged},
	{"DeleteSetting", testSettingsDelete},
	{"SinceFilter", testSettingsSince},
	{"RecreateClearsDeletedEntry", testSettingsRecreate},
	{"ExplicitVersionZeroOnExisting412", testSettingsVersionZero},
	{"ClientAheadOfSettingVersionAccepted", testSettingsClientAhead},
	{"Allows270PerRequest", testSettingsLimit},
}

func tagColors(color string, version int64) obj {
	return obj{"tagColors": obj{"value": []obj{{"name": "red", "color": color}}, "version": version}}
}

func testSettingsPostGet(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "settings", tagColors("#FF0000", 0))
	mustStatus(t, r, 204)
	v := versionHeader(t, r)
	r = l.get(t, "settings")
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if len(at[[]any](t, o, "tagColors", "value")) != 1 {
		t.Errorf("tagColors = %v", o["tagColors"])
	}
	if got := atInt(t, o, "tagColors", "version"); got != v {
		t.Errorf("setting version = %d, want library version %d", got, v)
	}
}

func testSettingsUnchanged(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "settings", tagColors("#FF0000", 0))
	mustStatus(t, r, 204)
	v := versionHeader(t, r)
	r = l.post(t, "settings", tagColors("#FF0000", v))
	mustStatus(t, r, 204)
	if got := versionHeader(t, r); got != v {
		t.Errorf("unchanged write bumped version %d -> %d", v, got)
	}
}

func testSettingsChanged(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "settings", tagColors("#FF0000", 0))
	v := versionHeader(t, r)
	r = l.post(t, "settings", tagColors("#00FF00", v))
	mustStatus(t, r, 204)
	if got := versionHeader(t, r); got <= v {
		t.Errorf("changed write did not bump: %d -> %d", v, got)
	}
}

func testSettingsDelete(t *testing.T, e *env) {
	l := e.userLib(t)
	v := versionHeader(t, l.post(t, "settings", tagColors("#FF0000", 0)))
	r := l.do(t, "DELETE", "settings?settingKey=tagColors", withIfUnmodified(v))
	mustStatus(t, r, 204)
	if got := versionHeader(t, r); got <= v {
		t.Errorf("delete did not bump: %d -> %d", v, got)
	}
	r = l.get(t, "settings")
	mustStatus(t, r, 200)
	if has(parseObj(t, r), "tagColors") {
		t.Error("deleted setting still returned")
	}
	r = l.get(t, "deleted?since=0")
	if got := strs(at[[]any](t, parseObj(t, r), "settings")); !slices.Contains(got, "tagColors") {
		t.Errorf("deleted.settings = %v", got)
	}
}

func testSettingsSince(t *testing.T, e *env) {
	l := e.userLib(t)
	v := versionHeader(t, l.post(t, "settings", tagColors("#FF0000", 0)))
	r := l.get(t, "settings?since="+strconv.FormatInt(v, 10))
	mustStatus(t, r, 200)
	if o := parseObj(t, r); len(o) != 0 {
		t.Errorf("since=%d returned %v, want empty", v, o)
	}
	r = l.get(t, "settings?since=0")
	if !has(parseObj(t, r), "tagColors") {
		t.Error("since=0 lacks tagColors")
	}
}

func testSettingsRecreate(t *testing.T, e *env) {
	l := e.userLib(t)
	v := versionHeader(t, l.post(t, "settings", tagColors("#FF0000", 0)))
	mustStatus(t, l.do(t, "DELETE", "settings?settingKey=tagColors", withIfUnmodified(v)), 204)
	mustStatus(t, l.post(t, "settings", tagColors("#0000FF", 0)), 204)
	r := l.get(t, "deleted?since=0")
	if got := strs(at[[]any](t, parseObj(t, r), "settings")); slices.Contains(got, "tagColors") {
		t.Errorf("recreated setting still in deleted log: %v", got)
	}
}

func testSettingsVersionZero(t *testing.T, e *env) {
	l := e.userLib(t)
	mustStatus(t, l.post(t, "settings", tagColors("#FF0000", 0)), 204)
	r := l.post(t, "settings", tagColors("#00FF00", 0))
	mustStatus(t, r, 412)
	r = l.get(t, "settings/tagColors")
	if got := at[[]any](t, parseObj(t, r), "value"); got[0].(obj)["color"] != "#FF0000" {
		t.Errorf("rejected write changed the setting: %v", got)
	}
}

func testSettingsClientAhead(t *testing.T, e *env) {
	l := e.userLib(t)
	v := versionHeader(t, l.post(t, "settings", tagColors("#FF0000", 0)))
	mustStatus(t, l.post(t, "settings", tagColors("#00FF00", v+50)), 204)
}

func testSettingsLimit(t *testing.T, e *env) {
	l := e.userLib(t)
	batch := obj{}
	for i := 0; i < 270; i++ {
		batch[fmt.Sprintf("s%d", i)] = obj{"value": i}
	}
	mustStatus(t, l.post(t, "settings", batch), 204)
	batch["one-too-many"] = obj{"value": 1}
	mustStatus(t, l.post(t, "settings", batch), 413)
}
