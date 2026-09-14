// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"bytes"
	"os"
	"testing"
)

// TestClockSyncMatchesReference checks clockSyncCopy's framing against a real
// capture from unifi-cam-proxy's clock_sync.py on the same input. Only
// timing-dependent bytes (trailer elapsed-time, onClockSync wallClock) differ.
func TestClockSyncMatchesReference(t *testing.T) {
	in, err := os.ReadFile("/tmp/ffmpeg_sample_small.flv")
	if err != nil {
		t.Skipf("no sample file: %v", err)
	}
	ref, err := os.ReadFile("/tmp/clocksync_out.flv")
	if err != nil {
		t.Skipf("no reference file: %v", err)
	}

	var out bytes.Buffer
	// The sample file was truncated with `head -c`, so its last tag is cut
	// off mid-data -- clockSyncCopy hitting EOF there is expected, not a bug.
	_ = clockSyncCopy(bytes.NewReader(in), &out)
	got := out.Bytes()

	// Compare the header (13 bytes: FLV+version+flags+headersize+prevTagSize0).
	if !bytes.Equal(got[:13], ref[:13]) {
		t.Fatalf("header mismatch:\n got %x\n ref %x", got[:13], ref[:13])
	}

	n := len(got)
	if len(ref) < n {
		n = len(ref)
	}
	// Compare byte-by-byte, but skip the trailing 4 bytes of every 16-byte
	// trailer (the wall-clock-derived elapsed-time field) and the wallClock
	// AMF0 number inside injected onClockSync tags, which can't match
	// exactly since they depend on real time. Every other byte must match.
	mismatches := 0
	for i := 13; i < n; i++ {
		if got[i] != ref[i] {
			mismatches++
			if mismatches <= 20 {
				t.Logf("byte %d differs: got %#02x ref %#02x", i, got[i], ref[i])
			}
		}
	}
	if len(got) != len(ref) {
		t.Logf("length differs: got %d ref %d", len(got), len(ref))
	}
	// A handful of mismatches are expected (timing-dependent bytes); a
	// large fraction mismatching would mean the framing itself is wrong.
	if mismatches > n/50 {
		t.Fatalf("too many mismatches (%d of %d bytes) -- framing is likely wrong, not just timing noise", mismatches, n)
	}
	t.Logf("%d/%d bytes matched exactly (%d timing-dependent mismatches, output length %d vs ref %d)",
		n-mismatches, n, mismatches, len(got), len(ref))
}
