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

// Protect repeats the full settings object every ChangeIspSettings, so defaults
// must not be forwarded (they'd perturb the stock/mediad look on connect).
func TestIspControlMapSkipsDefaults(t *testing.T) {
	out := ispControlMap(ispSettingsDefaults())
	if len(out) != 0 {
		t.Fatalf("defaults forwarded %d controls: %+v", len(out), out)
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
		{"wdr", 0, "wdr", 0},
		{"wdr", 2, "wdr", 255},
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

// A neutral brightness value must not be forwarded; a changed sibling must.
func TestIspControlMapPartial(t *testing.T) {
	payload := ispSettingsDefaults()
	payload["brightness"] = float64(50) // default: skipped
	payload["contrast"] = float64(51)   // changed: forwarded
	out := ispControlMap(payload)
	if _, ok := findCtl(t, out, "brightness"); ok {
		t.Fatalf("default brightness forwarded: %+v", out)
	}
	if _, ok := findCtl(t, out, "contrast"); !ok {
		t.Fatalf("changed contrast not forwarded: %+v", out)
	}
}
