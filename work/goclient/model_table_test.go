// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import "testing"

// The 8th model_table column (high_bitrate) pins a model's HIGH-channel
// bitrate; its absence must leave the controller in charge.
const sampleModelTable = `# model_table -- comment line
#
y623     gc3003  368  28  2304  1296  no  2200000
h52ga    gc2053  368  28  1920  1080  yes
r35gb    gc2053  0    0   1920  1080  yes   # inline comment
`

func TestParseModelDef(t *testing.T) {
	cases := []struct {
		name        string
		wantOK      bool
		wantW       int
		wantH       int
		wantPTZ     bool
		wantBitrate int
	}{
		{"y623", true, 2304, 1296, false, 2200000},
		{"h52ga", true, 1920, 1080, true, 0},
		{"r35gb", true, 1920, 1080, true, 0},
		{"y291ga", false, 2304, 1296, false, 0}, // unlisted -> conservative default
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := parseModelDef(sampleModelTable, tc.name)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if def.highWidth != tc.wantW || def.highHeight != tc.wantH {
				t.Fatalf("geometry = %dx%d, want %dx%d", def.highWidth, def.highHeight, tc.wantW, tc.wantH)
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
