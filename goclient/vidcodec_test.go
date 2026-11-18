package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A restarted client must recover the last codec per stream from disk instead
// of starting from h264 (which made the first partial ChangeVideoSettings flip
// the high channel to h264 on the encoder).
func TestVidCodecsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vidcodec")
	saveVidCodecs(p, map[string]string{"video1": "h265", "video2": "h265", "video3": "h264", "junk": "h265"})
	got := loadVidCodecs(p)
	if got["video1"] != "h265" || got["video2"] != "h265" || got["video3"] != "h264" {
		t.Fatalf("round trip: %v", got)
	}
	if _, ok := got["junk"]; ok {
		t.Fatalf("unknown key persisted: %v", got)
	}
	if m := loadVidCodecs(filepath.Join(t.TempDir(), "absent")); m != nil {
		t.Fatalf("missing file should load nil, got %v", m)
	}
}

// On an encoder path that cannot produce H.265 (the stock rmm encoder), every
// h265 entry must be rewritten to h264 so the bridge is never told "CODEC h265"
// over an H.264 elementary stream.
func TestForceH264(t *testing.T) {
	m := map[string]string{"video1": "h265", "video2": "h264", "video3": "h265"}
	forceH264(m)
	if m["video1"] != "h264" || m["video2"] != "h264" || m["video3"] != "h264" {
		t.Fatalf("forceH264: %v", m)
	}
}

func TestStreamCodecsRestored(t *testing.T) {
	old := vidCodecFilePath
	defer func() { vidCodecFilePath = old }()
	vidCodecFilePath = filepath.Join(t.TempDir(), "vidcodec")
	if err := os.WriteFile(vidCodecFilePath, []byte("video1=h265\nvideo2=h265\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	m := c.streamCodecs()
	if m["video1"] != "h265" || m["video2"] != "h265" || m["video3"] != "h264" {
		t.Fatalf("restored codecs: %v", m)
	}
	c.storeStreamCodecs(m) // first full store persists the (defaulted) third stream
	// Storing an unchanged map again must not rewrite the file.
	os.Remove(vidCodecFilePath)
	c.storeStreamCodecs(map[string]string{"video1": "h265", "video2": "h265", "video3": "h264"})
	if b, _ := os.ReadFile(vidCodecFilePath); len(b) != 0 {
		t.Fatalf("unchanged store rewrote the file: %q", b)
	}
	m["video3"] = "h265"
	c.storeStreamCodecs(m)
	if got := loadVidCodecs(vidCodecFilePath); got["video3"] != "h265" {
		t.Fatalf("changed store not persisted: %v", got)
	}
}

// TestH265AdvertisingNeedsExplicitOptIn pins the experimental gate: H.265 is
// never advertised without H265=yes, and never on the stock rmv path.
func TestH265AdvertisingNeedsExplicitOptIn(t *testing.T) {
	cases := []struct {
		experimental, mediadPath, want bool
	}{
		{false, false, false},
		{false, true, false}, // default: off on every camera
		{true, false, false}, // opt-in on the rmm path still cannot encode it
		{true, true, true},   // explicit opt-in + mediad
	}
	for _, tc := range cases {
		if got := h265Advertising(tc.experimental, tc.mediadPath); got != tc.want {
			t.Fatalf("h265Advertising(%v, %v) = %v, want %v",
				tc.experimental, tc.mediadPath, got, tc.want)
		}
	}
}
