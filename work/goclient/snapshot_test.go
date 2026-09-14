// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"testing"
	"time"
)

// TestSnapshotTimeout pins the clamping of the controller's timeoutMs. The
// bug this guards: a hardcoded 8s cap was shorter than the HIGH stream's ~9s
// IDR wait, so imggrabber was SIGKILLed mid-capture.
func TestSnapshotTimeout(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    time.Duration
	}{
		{"no timeout -> default", map[string]interface{}{}, defaultSnapshotTimeout},
		{"non-numeric ignored", map[string]interface{}{"timeoutMs": "soon"}, defaultSnapshotTimeout},
		{"zero ignored", map[string]interface{}{"timeoutMs": float64(0)}, defaultSnapshotTimeout},
		{"controller 60s clamped to cap", map[string]interface{}{"timeoutMs": float64(60000)}, maxSnapshotTimeout},
		{"short request floored", map[string]interface{}{"timeoutMs": float64(3000)}, minSnapshotTimeout},
		{"moderate request honored", map[string]interface{}{"timeoutMs": float64(20000)}, 20 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotTimeout(tc.payload); got != tc.want {
				t.Fatalf("snapshotTimeout(%v) = %s, want %s", tc.payload, got, tc.want)
			}
		})
	}
	if minSnapshotTimeout >= defaultSnapshotTimeout {
		t.Fatalf("min (%s) must be below default (%s)", minSnapshotTimeout, defaultSnapshotTimeout)
	}
	if maxSnapshotTimeout > 60*time.Second {
		t.Fatalf("cap (%s) must stay within the controller's observed 60s grant", maxSnapshotTimeout)
	}
}

// TestSnapshotCache checks the last-good fallback: stored bytes are copied
// (so later mutation can't corrupt the cache), the age is reported, and an
// unknown resolution misses.
func TestSnapshotCache(t *testing.T) {
	c := &Client{snapshotJPEG: map[string][]byte{}, snapshotAt: map[string]time.Time{}}

	if _, _, ok := c.cachedSnapshot("high"); ok {
		t.Fatal("empty cache should miss")
	}

	orig := []byte{0xff, 0xd8, 0xff, 0xe0}
	c.storeSnapshot("high", orig)
	orig[0] = 0x00 // must not affect the cached copy

	got, age, ok := c.cachedSnapshot("high")
	if !ok {
		t.Fatal("stored snapshot should hit")
	}
	if got[0] != 0xff {
		t.Fatalf("cached bytes were aliased, got %#02x want 0xff", got[0])
	}
	if age < 0 || age > time.Minute {
		t.Fatalf("implausible age %s", age)
	}

	if _, _, ok := c.cachedSnapshot("low"); ok {
		t.Fatal("per-resolution cache: low should still miss")
	}

	// Expiry: an entry older than the TTL must be reported as a miss.
	c.snapshotMu.Lock()
	c.snapshotAt["low"] = time.Now().Add(-snapshotCacheTTL - time.Second)
	c.snapshotJPEG["low"] = []byte{1}
	c.snapshotMu.Unlock()
	if _, _, ok := c.cachedSnapshot("low"); ok {
		t.Fatal("stale entry should miss")
	}
}
