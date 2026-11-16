// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"testing"
	"time"
)

func TestPosixForZone(t *testing.T) {
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		raw  string
		now  time.Time
		want string
	}{
		{"Europe/Berlin", winter, "CET-1"},
		{"Europe/Berlin", summer, "CEST-2"},
		{"America/New_York", winter, "EST5"},
		{"America/New_York", summer, "EDT4"},
		{"Asia/Kolkata", winter, "IST-5:30"},
		{"Australia/Adelaide", summer, "ACST-9:30"},
		{"Australia/Adelaide", winter, "ACDT-10:30"},
		{"UTC", winter, "UTC0"},
		{"Etc/GMT+3", winter, "<-03>3"},
		// Older controllers send the POSIX rule itself; pass it through.
		{"PST8PDT,M3.2.0,M11.1.0", winter, "PST8PDT,M3.2.0,M11.1.0"},
		{"CET-1CEST,M3.5.0,M10.5.0/3", summer, "CET-1CEST,M3.5.0,M10.5.0/3"},
	}
	for _, tc := range cases {
		got, err := posixForZone(tc.raw, tc.now)
		if err != nil {
			t.Errorf("posixForZone(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("posixForZone(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestPosixForZoneRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{"", "Europe/Nowhere", "Europe\nBerlin", "not a zone", "GMT0 extra"} {
		if got, err := posixForZone(raw, now); err == nil {
			t.Errorf("posixForZone(%q) = %q, want error", raw, got)
		}
	}
}

func TestPosixOffset(t *testing.T) {
	cases := map[int]string{
		0:      "0",
		3600:   "-1",
		-18000: "5",
		19800:  "-5:30",
		-12600: "3:30",
		20700:  "-5:45",
		-5400:  "1:30",
	}
	for off, want := range cases {
		if got := posixOffset(off); got != want {
			t.Errorf("posixOffset(%d) = %q, want %q", off, got, want)
		}
	}
}
