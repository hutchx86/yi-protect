// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import "testing"

func findCtl(t *testing.T, out []mediadCtl, key string) (int, bool) {
	t.Helper()
	for _, c := range out {
		if c.key == key {
			return c.value, true
		}
	}
	return 0, false
}

// ispControlMap emits every mapped field (including defaults); suppression of
// unchanged fields is the delta layer's job, so the mapper must not drop them.
func TestIspControlMapEmitsMappedFields(t *testing.T) {
	out := ispControlMap(ispSettingsDefaults())
	for _, key := range []string{"brightness", "contrast", "hue", "saturation", "sharpness", "denoise", "tdf", "wdr", "mirror", "flip"} {
		if _, ok := findCtl(t, out, key); !ok {
			t.Errorf("key %s missing from default mapping: %+v", key, out)
		}
	}
}

// The delta must suppress a whole changeset, not just compare to static
// defaults: the first object after a connect is seeded (recorded, not sent),
// then only genuine changes are forwarded.
func TestMediadDeltaSeedsFirstObject(t *testing.T) {
	d := newMediadDelta(true)
	if d.changed(mediadCtl{"brightness", 36}) {
		t.Fatal("first object must be seeded, not sent")
	}
	if d.changed(mediadCtl{"contrast", 100}) {
		t.Fatal("first object must be seeded, not sent")
	}
	d.seed = false // end of the first object
	if d.changed(mediadCtl{"brightness", 36}) {
		t.Fatal("unchanged value after seed must be skipped")
	}
	if !d.changed(mediadCtl{"brightness", 60}) {
		t.Fatal("changed value after seed must be forwarded")
	}
}

// A partial object first (the legacy ChangeBrightnessSettings) must not end the
// seed, or the following full ChangeIspSettings would apply every field the
// partial object omitted.
func TestMediadDeltaPartialThenFullSeed(t *testing.T) {
	d := newMediadDelta(true)
	if got := d.filter([]mediadCtl{{"brightness", 36}}, false); len(got) != 0 {
		t.Fatalf("partial object must be seeded: %+v", got)
	}
	if !d.seed {
		t.Fatal("partial object must not end the seed")
	}
	full := []mediadCtl{{"brightness", 47}, {"contrast", 46}, {"saturation", 57}}
	if got := d.filter(full, true); len(got) != 0 {
		t.Fatalf("full object must be seeded: %+v", got)
	}
	if d.seed {
		t.Fatal("full object must end the seed")
	}
	if got := d.filter(full, true); len(got) != 0 {
		t.Fatalf("unchanged object must be skipped: %+v", got)
	}
	got := d.filter([]mediadCtl{{"brightness", 60}, {"contrast", 46}}, true)
	if len(got) != 1 || got[0].key != "brightness" {
		t.Fatalf("expected only brightness, got %+v", got)
	}
}

// A daemon (re)appearance forces one full apply on the next complete object,
// and that must survive the connect reset that follows a fresh client start.
func TestMediadDeltaPendingFullApply(t *testing.T) {
	d := newMediadDelta(true)
	d.reset(false) // daemon appeared -> pending full apply
	d.reset(true)  // connect reset must not cancel the pending apply
	if !d.pending {
		t.Fatal("connect reset cancelled the pending daemon apply")
	}
	// A partial object first must be recorded, not sent.
	if got := d.filter([]mediadCtl{{"brightness", 36}}, false); len(got) != 0 {
		t.Fatalf("partial during pending must not send: %+v", got)
	}
	full := []mediadCtl{{"brightness", 47}, {"contrast", 46}, {"saturation", 57}}
	got := d.filter(full, true)
	if len(got) != len(full) {
		t.Fatalf("pending full apply should send all %d, got %+v", len(full), got)
	}
	for _, c := range got {
		d.record(c)
	}
	if d.pending {
		t.Fatal("pending must clear after a complete object")
	}
	if got := d.filter(full, true); len(got) != 0 {
		t.Fatalf("expected dedupe after full apply, got %+v", got)
	}
}

// Without a seed (e.g. after a daemon restart / ResetIspSettings) the first
// sight of a value is applied, then deduped.
func TestMediadDeltaUnseededAppliesThenDedupes(t *testing.T) {
	d := newMediadDelta(false)
	if !d.changed(mediadCtl{"nightvision", 2}) {
		t.Fatal("unseeded first value must be applied")
	}
	d.record(mediadCtl{"nightvision", 2})
	if d.changed(mediadCtl{"nightvision", 2}) {
		t.Fatal("repeat of applied value must be skipped")
	}
	if !d.changed(mediadCtl{"nightvision", 1}) {
		t.Fatal("changed value must be applied")
	}
}

func TestIspControlMapScaling(t *testing.T) {
	cases := []struct {
		field string
		value float64
		key   string
		want  int
	}{
		{"brightness", 100, "brightness", 100},
		{"brightness", 0, "brightness", 0},
		{"brightness", 60, "brightness", 60},
		{"contrast", 60, "contrast", 60},
		{"hue", 100, "hue", 100},
		{"saturation", 100, "saturation", 100},
		{"sharpness", 100, "sharpness", 10},
		{"sharpness", 0, "sharpness", 0},
		{"denoise", 0, "denoise", 0},
		{"enable3dnr", 0, "tdf", 0},
		{"enable3dnr", 1, "tdf", 1},
		{"wdr", 0, "wdr", 0},
		{"wdr", 1, "wdr", 0},
		{"wdr", 2, "wdr", 128},
		{"wdr", 3, "wdr", 255},
		{"wdr", 0, "pltm", 0},
		{"wdr", 1, "pltm", 1},
		{"wdr", 2, "pltm", 1},
		{"mirror", 1, "mirror", 1},
		{"flip", 1, "flip", 1},
	}
	for _, c := range cases {
		payload := ispSettingsDefaults()
		payload[c.field] = c.value
		out := ispControlMap(payload)
		got, ok := findCtl(t, out, c.key)
		if !ok {
			t.Errorf("%s=%v: key %s not forwarded (out=%+v)", c.field, c.value, c.key, out)
			continue
		}
		if got != c.want {
			t.Errorf("%s=%v: %s=%d, want %d", c.field, c.value, c.key, got, c.want)
		}
	}
}

// A slider move that changes one field must forward only that field's control,
// even though the controller resent the whole object.
func TestIspControlMapPartialDelta(t *testing.T) {
	d := newMediadDelta(true)
	first := ispSettingsDefaults()
	first["brightness"] = float64(50)
	first["contrast"] = float64(50)
	for _, c := range ispControlMap(first) {
		d.changed(c) // seed
	}
	d.seed = false // end of the first object
	second := ispSettingsDefaults()
	second["brightness"] = float64(50) // unchanged
	second["contrast"] = float64(51)   // changed
	var forwarded []string
	for _, c := range ispControlMap(second) {
		if d.changed(c) {
			forwarded = append(forwarded, c.key)
		}
	}
	if len(forwarded) != 1 || forwarded[0] != "contrast" {
		t.Fatalf("expected only contrast forwarded, got %v", forwarded)
	}
}

// HDR is a PLTM module enable plus a strength: a 1->2 change is a strength-only
// change (forward wdr), while a 1->0 change flips the module (forward pltm).
func TestIspControlMapWdrSplitsPltmAndStrength(t *testing.T) {
	seed := func(d *mediadDelta, payload map[string]interface{}) {
		for _, c := range ispControlMap(payload) {
			d.changed(c)
		}
		d.seed = false
	}
	forwarded := func(d *mediadDelta, payload map[string]interface{}) []string {
		var out []string
		for _, c := range ispControlMap(payload) {
			if d.changed(c) {
				out = append(out, c.key)
			}
		}
		return out
	}

	stock := ispSettingsDefaults() // wdr=1 => pltm=1 + wdr=0

	d := newMediadDelta(true)
	seed(d, stock)
	second := ispSettingsDefaults()
	second["wdr"] = float64(2)
	if got := forwarded(d, second); len(got) != 1 || got[0] != "wdr" {
		t.Fatalf("wdr 1->2 should forward only wdr, got %v", got)
	}

	d = newMediadDelta(true)
	seed(d, stock)
	off := ispSettingsDefaults()
	off["wdr"] = float64(0)
	if got := forwarded(d, off); len(got) != 1 || got[0] != "pltm" {
		t.Fatalf("wdr 1->0 should forward only pltm, got %v", got)
	}
}

func TestCustomValueToLux(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0, 1}, {10, 30}, {2, 7}, {5, 16}, {1, 4},
	}
	for _, c := range cases {
		if got := customValueToLux(c.in); got != c.want {
			t.Errorf("customValueToLux(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestNightVisionControls(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    []mediadCtl
	}{
		{"manual on", map[string]interface{}{"irLedMode": "manual", "irLedLevel": float64(215)},
			[]mediadCtl{{"nightvision", 1}}},
		{"manual off", map[string]interface{}{"irLedMode": "manual", "irLedLevel": float64(0)},
			[]mediadCtl{{"nightvision", 0}}},
		{"auto sensitivity", map[string]interface{}{"irLedMode": "auto", "irLedLevel": float64(1), "icrSwitchMode": "sensitivity"},
			[]mediadCtl{{"nightvision", 2}}},
		{"auto custom", map[string]interface{}{"irLedMode": "auto", "irLedLevel": float64(1), "icrSwitchMode": "lux", "icrCustomValue": float64(2)},
			[]mediadCtl{{"nightvision", 2}, {"night_lux", 7}}},
		{"auto filter-only", map[string]interface{}{"irLedMode": "auto", "irLedLevel": float64(0), "icrSwitchMode": "lux", "icrCustomValue": float64(2)},
			[]mediadCtl{{"nightvision", 2}, {"night_lux", 7}, {"ir_led", 0}}},
		{"external ir", map[string]interface{}{"irLedMode": "auto", "irLedLevel": float64(1), "icrSwitchMode": "sensitivity", "enableExternalIr": float64(1)},
			[]mediadCtl{{"nightvision", 2}, {"ir_led", 0}}},
		{"no ir fields", map[string]interface{}{"brightness": float64(60)}, nil},
	}
	for _, c := range cases {
		got := nightVisionControls(c.payload)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: [%d] got %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestShutterControls(t *testing.T) {
	cases := []struct {
		mode string
		want int
	}{
		{"default", 0}, {"sport", 1}, {"slowShutter", 2},
	}
	for _, c := range cases {
		got := shutterControls(map[string]interface{}{"videoMode": c.mode})
		if len(got) != 1 || got[0].key != "shutter" || got[0].value != c.want {
			t.Errorf("videoMode=%s: got %+v, want shutter=%d", c.mode, got, c.want)
		}
	}
	if got := shutterControls(map[string]interface{}{"videoMode": "highFps"}); got != nil {
		t.Errorf("unknown videoMode forwarded: %+v", got)
	}
	if got := shutterControls(map[string]interface{}{}); got != nil {
		t.Errorf("absent videoMode forwarded: %+v", got)
	}
}

// A key the daemon reports as config/webui-only is learned at runtime (the
// contract can differ per build) and dropped from later objects.
func TestMediadDropLocked(t *testing.T) {
	mediadLockedMu.Lock()
	mediadLocked = map[string]bool{}
	mediadLockedMu.Unlock()

	in := []mediadCtl{{"wdr", 128}, {"brightness", 50}}
	if got := mediadDropLocked(in); len(got) != 2 {
		t.Fatalf("nothing locked yet, got %+v", got)
	}
	mediadMarkLocked("wdr")
	got := mediadDropLocked(in)
	if len(got) != 1 || got[0].key != "brightness" {
		t.Fatalf("expected only brightness after locking wdr, got %+v", got)
	}
}
