// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import "testing"

// TestResolveHighBitrate pins the precedence: a per-unit mediad.conf pin wins
// over the model table, and a model with no policy still follows the controller.
func TestResolveHighBitrate(t *testing.T) {
	cases := []struct {
		name                            string
		modelPin, daemonPin, daemonRate int
		want                            int
	}{
		{"model policy only", 2800000, 0, 0, 2800000},
		{"daemon pin overrides model", 2800000, 1, 6000000, 6000000},
		{"daemon pin on a follow model", 0, 1, 6000000, 6000000},
		{"invalid daemon rate falls back", 2800000, 1, 0, 2800000},
		{"unpinned follow model", 0, 0, 3000000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveHighBitrate(tc.modelPin, tc.daemonPin, tc.daemonRate); got != tc.want {
				t.Fatalf("resolveHighBitrate(%d, %d, %d) = %d, want %d",
					tc.modelPin, tc.daemonPin, tc.daemonRate, got, tc.want)
			}
		})
	}
}

// TestDeclaredHighBitrate pins what Protect is shown: an explicit pin, then
// the controller's target, then the daemon's actual rate, then the capability.
func TestDeclaredHighBitrate(t *testing.T) {
	cases := []struct {
		name                        string
		pinned, controller, current int
		want                        int
	}{
		{"explicit pin wins", 2800000, 6000000, 0, 2800000},
		{"controller target clamped to the advertised max", 0, 6000000, 3000000, mediaBitrateMax},
		{"daemon actual when video1 is absent", 0, 0, 3000000, 3000000},
		{"initial advertise is the capability", 0, 0, 0, mediaBitrateMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaredHighBitrate(tc.pinned, tc.controller, tc.current); got != tc.want {
				t.Fatalf("declaredHighBitrate(%d, %d, %d) = %d, want %d",
					tc.pinned, tc.controller, tc.current, got, tc.want)
			}
		})
	}
}
