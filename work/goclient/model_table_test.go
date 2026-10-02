// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import "testing"

// model_table columns: model sensor ring_offset ring_header high_w high_h ptz
// [high_bitrate] [mediad_w mediad_h]. The 8th column (high_bitrate) opts a
// model out of following the controller; the 9th/10th (mediad geometry) are the
// geometry the mediad encoder streams, used when the camera is on the mediad
// path.
const sampleModelTable = `# model_table -- comment line
#
y623     gc3003  368  28  2304  1296  no   2200000  2304  1296
h51ga    gc2053  368  28  2304  1296  yes  0        1920  1080
h52ga    gc2053  368  28  1920  1080  yes
r35gb    gc2053  0    0   1920  1080  yes   # inline comment (no mediad cols)
`

func TestParseModelDef(t *testing.T) {
	cases := []struct {
		name        string
		wantOK      bool
		wantW       int
		wantH       int
		wantMediadW int
		wantMediadH int
		wantPTZ     bool
		wantBitrate int
	}{
		{"y623", true, 2304, 1296, 2304, 1296, false, 2200000},
		{"h51ga", true, 2304, 1296, 1920, 1080, true, 0},
		{"h52ga", true, 1920, 1080, 0, 0, true, 0},    // no mediad columns
		{"r35gb", true, 1920, 1080, 0, 0, true, 0},    // inline comment
		{"y291ga", false, 2304, 1296, 0, 0, false, 0}, // unlisted -> conservative default
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := parseModelDef(sampleModelTable, tc.name)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if def.highWidth != tc.wantW || def.highHeight != tc.wantH {
				t.Fatalf("high geometry = %dx%d, want %dx%d", def.highWidth, def.highHeight, tc.wantW, tc.wantH)
			}
			if def.mediadWidth != tc.wantMediadW || def.mediadHeight != tc.wantMediadH {
				t.Fatalf("mediad geometry = %dx%d, want %dx%d", def.mediadWidth, def.mediadHeight, tc.wantMediadW, tc.wantMediadH)
			}
			if def.ptz != tc.wantPTZ {
				t.Fatalf("ptz = %v, want %v", def.ptz, tc.wantPTZ)
			}
			if def.highBitrate != tc.wantBitrate {
				t.Fatalf("highBitrate = %d, want %d", def.highBitrate, tc.wantBitrate)
			}
		})
	}
}

// The declared geometry must follow the ACTIVE encoder: stock-rmm high_w/h on
// the rmm path, mediad_w/h on the mediad path, falling back to high_w/h when a
// row has no mediad columns.
func TestModelGeometrySelection(t *testing.T) {
	h, _ := parseModelDef(sampleModelTable, "h51ga")
	if w, hh := h.geometry(false); w != 2304 || hh != 1296 {
		t.Fatalf("h51ga rmm geometry = %dx%d, want 2304x1296", w, hh)
	}
	if w, hh := h.geometry(true); w != 1920 || hh != 1080 {
		t.Fatalf("h51ga mediad geometry = %dx%d, want 1920x1080", w, hh)
	}
	// No mediad columns -> both paths use high_w/h.
	y, _ := parseModelDef(sampleModelTable, "y623")
	if w, hh := y.geometry(true); w != 2304 || hh != 1296 {
		t.Fatalf("y623 mediad fallback = %dx%d, want 2304x1296", w, hh)
	}
}
