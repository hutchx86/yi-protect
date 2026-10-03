package main

// Persistence of the codec each video stream last carried (h264 / h265). Kept
// apart from the Protect message handling in main.go: a restarted client starts
// from this state instead of h264, so its first (partial) ChangeVideoSettings
// neither advertises h264 nor tells mediad to drop a channel that is on h265.

import (
	"log"
	"os"
	"strings"
)

// vidCodecFilePath persists the last codec per stream across avclient restarts.
// Without it a fresh client starts from h264 and its first (partial) settings
// object tells mediad to drop a channel that is really on h265; the resulting
// switch reconnects every stream and races the controller's stream bookkeeping.
var vidCodecFilePath = yipPrefix + "/etc/yi_protect_client_go.vidcodec"

func loadVidCodecs(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) == 2 && (kv[1] == "h264" || kv[1] == "h265") &&
			(kv[0] == "video1" || kv[0] == "video2" || kv[0] == "video3") {
			m[kv[0]] = kv[1]
		}
	}
	return m
}

// forceH264 rewrites any h265 entry to h264 in place. Used when the active
// encoder path (the stock rmm encoder) cannot produce H.265, so a persisted or
// controller-requested h265 is never forwarded to the bridge as HEVC over an
// H.264 elementary stream (which latches the bridge's drop-until-keyframe).
func forceH264(m map[string]string) {
	for k, v := range m {
		if v == "h265" {
			m[k] = "h264"
		}
	}
}

func saveVidCodecs(path string, m map[string]string) {
	var sb strings.Builder
	for _, k := range []string{"video1", "video2", "video3"} {
		if v := m[k]; v == "h264" || v == "h265" {
			sb.WriteString(k + "=" + v + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		log.Printf("failed to persist stream codecs: %v", err)
	}
}
