// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// mediad_ctl.go - optional client for the custom rmm replacement (mediad): with
// IS_MEDIAD=yes and a responsive mediad, picture controls go to its socket.
package main

import (
	"bufio"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mediadSockPath is mediad's control socket (MEDIAD_CTL_SOCK overrides, matching
// the daemon: os.Getenv then /tmp/mediad_ctl.sock).
func mediadSockPath() string {
	if p := os.Getenv("MEDIAD_CTL_SOCK"); p != "" {
		return p
	}
	return "/tmp/mediad_ctl.sock"
}

// mediadBinaryCandidates are the install locations a mediad build may land at,
// used only to classify "installed but not running" in the logs.
var mediadBinaryCandidates = []string{
	unifiPrefix + "/bin/mediad",
	unifiPrefix + "/bin/mediad_2019",
	unifiPrefix + "/bin/mediad_rtos_v",
	unifiPrefix + "/bin/mediad_rtos",
	"/home/app/mediad",
	"/home/app/mediad_2019",
	"/home/app/mediad_rtos_v",
	"/home/app/mediad_rtos",
}

// findMediadProcess scans /proc for a running mediad (comm prefix "mediad",
// excluding the "mediad_ctl" CLI) and returns pid and resolved exe path.
func findMediadProcess() (pid int, exe string, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if !strings.HasPrefix(name, "mediad") || strings.HasPrefix(name, "mediad_ctl") {
			continue
		}
		exe, _ = os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		return p, exe, true
	}
	return 0, "", false
}

// mediadInstalled reports whether a mediad binary is present, and its path;
// a running mediad's own exe is authoritative, else the known locations.
func mediadInstalled() (string, bool) {
	if _, exe, ok := findMediadProcess(); ok && exe != "" {
		if fi, err := os.Stat(exe); err == nil && fi.Mode().IsRegular() {
			return exe, true
		}
	}
	for _, p := range mediadBinaryCandidates {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// mediadCommand sends one line to the control socket and returns the reply.
func mediadCommand(cmd string) (string, error) {
	conn, err := net.DialTimeout("unix", mediadSockPath(), 500*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// mediadPing round-trips the socket's `ping` command; this is the "running and
// responsive" half of the availability check.
func mediadPing() bool {
	reply, err := mediadCommand("ping")
	return err == nil && strings.HasPrefix(reply, "ok")
}

// mediad availability is cached briefly so a settings burst doesn't rescan /proc
// per message; a failed send clears the cache so a killed mediad is noticed.
var (
	mediadMu        sync.Mutex
	mediadReady     bool
	mediadCheckedAt time.Time
)

const mediadRecheckInterval = 30 * time.Second

// mediadInvalidate forces the next mediadEnabled() to re-detect.
func mediadInvalidate() {
	mediadMu.Lock()
	mediadCheckedAt = time.Time{}
	mediadMu.Unlock()
}

// mediadEnabled is the single gate for every advanced path: IS_MEDIAD set AND a
// running mediad with a responsive control socket.
func mediadEnabled() bool {
	if !cfg.IsMediad {
		return false
	}

	mediadMu.Lock()
	defer mediadMu.Unlock()
	if !mediadCheckedAt.IsZero() && time.Since(mediadCheckedAt) < mediadRecheckInterval {
		return mediadReady
	}
	mediadCheckedAt = time.Now()

	exe, installed := mediadInstalled()
	pid, _, running := findMediadProcess()
	responsive := running && mediadPing()
	ready := installed && running && responsive

	if ready != mediadReady {
		if ready {
			log.Printf("mediad: advanced controls ENABLED (pid %d, %s)", pid, exe)
			// A (re)appeared daemon starts at its defaults: re-send the
			// controller's last-known values now (async: mediadSet may take
			// mediadMu via mediadInvalidate).
			go mediadReapplyKnown()
		} else {
			log.Printf("mediad: advanced controls DISABLED (installed=%v running=%v responsive=%v)", installed, running, responsive)
		}
	}
	mediadReady = ready
	return mediadReady
}

// logMediadStatus logs the startup state once, so an IS_MEDIAD=yes deployment
// against a missing/stopped mediad is visible rather than silent.
func logMediadStatus() {
	if !cfg.IsMediad {
		return
	}
	exe, installed := mediadInstalled()
	pid, _, running := findMediadProcess()
	log.Printf("IS_MEDIAD=yes: installed=%v (%s) running=%v (pid %d) socket=%s", installed, exe, running, pid, mediadSockPath())
	// Prime the availability cache now so the false->true transition (which
	// re-arms the delta) happens here, before run() seeds the first object.
	mediadEnabled()
	go mediadWatch(pid)
}

// mediadWatch notices a restarted daemon (new PID). A restart takes a few
// seconds, well inside the 30 s availability cache, so without this the
// client never saw it go away and the daemon stayed at its defaults until the
// controller happened to send a new settings object.
func mediadWatch(lastPid int) {
	for {
		time.Sleep(5 * time.Second)
		pid, _, ok := findMediadProcess()
		if !ok || pid == lastPid {
			continue // keep lastPid while it is down so the restart is seen
		}
		prev := lastPid
		lastPid = pid
		for i := 0; i < 10 && !mediadPing(); i++ {
			time.Sleep(time.Second)
		}
		log.Printf("mediad: new daemon (pid %d -> %d), re-applying controller settings", prev, pid)
		// Force the not-ready -> ready transition, which re-applies.
		mediadMu.Lock()
		mediadReady = false
		mediadCheckedAt = time.Time{}
		mediadMu.Unlock()
		mediadEnabled()
	}
}

// mediadReapplyKnown re-sends the controller's last-known values (applied or
// seeded) to a daemon that started at its defaults; Protect does not resend
// settings by itself. A category with nothing known yet applies its next
// complete object in full instead.
func mediadReapplyKnown() {
	var toSend []mediadCtl
	mediadDeltaMu.Lock()
	for _, d := range []*mediadDelta{mediadIspDelta, mediadVidDelta, mediadOsdDelta} {
		if len(d.last) == 0 {
			d.reset(false)
			continue
		}
		toSend = append(toSend, d.known()...)
	}
	mediadDeltaMu.Unlock()
	for _, c := range mediadDropLocked(toSend) {
		if err := mediadSet(c.key, c.value); err != nil {
			log.Printf("mediad ctl: reapply %s=%d: %v", c.key, c.value, err)
		} else {
			log.Printf("mediad ctl: reapply %s=%d ok", c.key, c.value)
		}
	}
}

// Some controls are config/webui-only; the daemon's `list` isn't reliable, so we
// learn from the error and stop sending that key for the process lifetime.
var (
	mediadLockedMu sync.Mutex
	mediadLocked   = map[string]bool{}
)

func mediadMarkLocked(key string) {
	mediadLockedMu.Lock()
	if !mediadLocked[key] {
		mediadLocked[key] = true
		log.Printf("mediad: %q is config/webui-only, not settable over the socket -- will not send it again", key)
	}
	mediadLockedMu.Unlock()
}

func mediadSet(key string, value int) error {
	reply, err := mediadCommand(fmt.Sprintf("set %s %d", key, value))
	if err != nil {
		mediadInvalidate()
		return err
	}
	if strings.HasPrefix(reply, "ok ") {
		return nil
	}
	if strings.Contains(reply, "config/webui-only") {
		mediadMarkLocked(key)
	}
	return fmt.Errorf("mediad: %s", reply)
}

// scaleLinear maps a Protect field linearly onto [lo,hi]. For symmetric ranges
// (brightness/contrast/hue) 50 lands on the neutral midpoint.
func scaleLinear(v float64, lo, hi int) int {
	r := math.Round(float64(lo) + v/100*float64(hi-lo))
	if r < float64(lo) {
		r = float64(lo)
	}
	if r > float64(hi) {
		r = float64(hi)
	}
	return int(r)
}

// mediadDelta tracks the value last seen per key so repeated settings objects
// only re-issue changes; seed records the first post-connect object unapplied.
type mediadDelta struct {
	last    map[string]int
	seed    bool
	pending bool
}

func newMediadDelta(seed bool) *mediadDelta {
	return &mediadDelta{last: map[string]int{}, seed: seed}
}

// changed reports whether c should be forwarded, recording it on the seed pass
// and for unchanged values. Caller holds mediadDeltaMu.
func (d *mediadDelta) changed(c mediadCtl) bool {
	if d.seed {
		d.last[c.key] = c.value
		return false
	}
	if prev, ok := d.last[c.key]; ok && prev == c.value {
		return false
	}
	return true
}

// record notes a successfully applied value. Caller holds mediadDeltaMu.
func (d *mediadDelta) record(c mediadCtl) { d.last[c.key] = c.value }

// known returns the last-known value of every key, sorted by key. Caller holds
// mediadDeltaMu.
func (d *mediadDelta) known() []mediadCtl {
	keys := make([]string, 0, len(d.last))
	for k := range d.last {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]mediadCtl, 0, len(keys))
	for _, k := range keys {
		out = append(out, mediadCtl{k, d.last[k]})
	}
	return out
}

// filter returns the subset of controls to forward, ending the seed once a
// complete object has been seen. Caller holds mediadDeltaMu when shared.
func (d *mediadDelta) filter(controls []mediadCtl, complete bool) []mediadCtl {
	if d.pending {
		if !complete {
			for _, c := range controls {
				d.last[c.key] = c.value
			}
			return nil
		}
		d.pending = false
		d.seed = false
		return controls
	}
	var out []mediadCtl
	for _, c := range controls {
		if d.changed(c) {
			out = append(out, c)
		}
	}
	if complete {
		d.seed = false
	}
	return out
}

// reset re-arms the delta. reset(true) is the connect reset (seed the next
// object); reset(false) forces a full apply on the next complete object.
func (d *mediadDelta) reset(seed bool) {
	d.last = map[string]int{}
	d.seed = seed
	if !seed {
		d.pending = true
	}
}

var (
	mediadDeltaMu sync.Mutex
	// Separate deltas for the three message categories: the controller seeds
	// each independently on connect, so one must not clear another's baseline.
	mediadIspDelta = newMediadDelta(true) // ChangeIspSettings (picture + night vision)
	mediadVidDelta = newMediadDelta(true) // ChangeVideoSettings (shutter + bitrate)
	mediadOsdDelta = newMediadDelta(true) // ChangeOsdSettings (burned-in overlay)
)

// resetMediadDelta drops the delta state and seeds the next object of each
// category; called on every WSS connect so the full resend is not re-applied.
func resetMediadDelta() {
	mediadDeltaMu.Lock()
	mediadIspDelta.reset(true)
	mediadVidDelta.reset(true)
	mediadOsdDelta.reset(true)
	mediadDeltaMu.Unlock()
}

// mediadDeltaReapply forces a full apply on the next complete object, used
// after ResetIspSettings and when a mediad daemon (re)appears.
func mediadDeltaReapply() {
	mediadDeltaMu.Lock()
	mediadIspDelta.reset(false)
	mediadVidDelta.reset(false)
	mediadOsdDelta.reset(false)
	mediadDeltaMu.Unlock()
}

// mediadApplyControls forwards only the controls whose value changed since the
// previous object of the same category; `complete` ends the seed.
func mediadApplyControls(controls []mediadCtl, d *mediadDelta, complete bool) {
	if len(controls) == 0 || !mediadEnabled() {
		return
	}
	// Drop controls the daemon has reported as config/webui-only before the
	// delta sees them, so a locked key isn't recorded as applied.
	controls = mediadDropLocked(controls)
	mediadDeltaMu.Lock()
	toSend := d.filter(controls, complete)
	mediadDeltaMu.Unlock()
	for _, c := range toSend {
		if err := mediadSet(c.key, c.value); err != nil {
			log.Printf("mediad ctl: %s=%d: %v", c.key, c.value, err)
		} else {
			mediadDeltaMu.Lock()
			d.record(c)
			mediadDeltaMu.Unlock()
			log.Printf("mediad ctl: %s=%d ok", c.key, c.value)
		}
	}
}

// mediadDropLocked removes controls the daemon reported as config/webui-only.
func mediadDropLocked(controls []mediadCtl) []mediadCtl {
	mediadLockedMu.Lock()
	defer mediadLockedMu.Unlock()
	if len(mediadLocked) == 0 {
		return controls
	}
	out := make([]mediadCtl, 0, len(controls))
	for _, c := range controls {
		if !mediadLocked[c.key] {
			out = append(out, c)
		}
	}
	return out
}

// mediadApplyIspSettings forwards the ChangeIspSettings subset: picture
// controls plus night-vision. `complete` is false for legacy Brightness.
func mediadApplyIspSettings(payload map[string]interface{}, complete bool) {
	controls := ispControlMap(payload)
	controls = append(controls, nightVisionControls(payload)...)
	mediadApplyControls(controls, mediadIspDelta, complete)
}

// osdControlMap maps ChangeOsdSettings to mediad's OSD controls; Protect nests
// per-stream objects under "_1".."_4", so the first one drives mediad's config.
func osdControlMap(payload map[string]interface{}) []mediadCtl {
	var out []mediadCtl

	src := payload
	for _, k := range []string{"_1", "_2", "_3", "_4"} {
		if sub, ok := payload[k].(map[string]interface{}); ok {
			src = sub
			break
		}
	}

	addBool := func(field, key string) {
		switch v := src[field].(type) {
		case bool:
			iv := 0
			if v {
				iv = 1
			}
			out = append(out, mediadCtl{key, iv})
		case float64:
			iv := 0
			if v != 0 {
				iv = 1
			}
			out = append(out, mediadCtl{key, iv})
		}
	}
	addScale := func(field, key string) {
		v, ok := payload[field].(float64)
		if !ok {
			return
		}
		iv := int(v)
		if iv < 0 {
			iv = 0
		}
		if iv > 100 {
			iv = 100
		}
		out = append(out, mediadCtl{key, iv})
	}

	// enableOverlay is the top-level master switch (mirrors this client's own
	// ChangeOsdSettings response shape); the rest are per-stream fields.
	addBool("enableOverlay", "osd")
	addBool("enableDate", "osd_date")
	addBool("enableLogo", "osd_logo")
	addBool("enableStreamerStatsLevel", "osd_bitrate")
	// Protect has no name toggle: "show name" sends the camera name in "tag",
	// off sends "". The name text itself comes from the persisted device name.
	if tag, ok := src["tag"].(string); ok {
		iv := 0
		if tag != "" {
			iv = 1
		}
		out = append(out, mediadCtl{"osd_name", iv})
	}
	addScale("textScale", "osd_text_scale")
	addScale("logoScale", "osd_logo_scale")
	if v, ok := payload["overlayColorId"].(float64); ok {
		out = append(out, mediadCtl{"osd_color", int(v)})
	}

	return out
}

func mediadApplyOsdSettings(payload map[string]interface{}) {
	mediadApplyControls(osdControlMap(payload), mediadOsdDelta, true)
}

// customValueToLux maps Protect's icrCustomValue (0-10, the "Custom" night
// vision threshold dial) onto mediad's night_lux, a 1-30 lux threshold.
func customValueToLux(v float64) int {
	lux := 1 + int(math.Round(v/10*29))
	if lux < 1 {
		lux = 1
	}
	if lux > 30 {
		lux = 30
	}
	return lux
}

// nightVisionControls maps Protect's night-vision fields (irLedMode, icrSwitchMode,
// icrCustomValue, enableExternalIr) to mediad's nightvision/night_lux/ir_led.
func nightVisionControls(payload map[string]interface{}) []mediadCtl {
	mode, ok := payload["irLedMode"].(string)
	if !ok {
		return nil
	}
	level, _ := payload["irLedLevel"].(float64)
	switchMode, _ := payload["icrSwitchMode"].(string)
	custom, _ := payload["icrCustomValue"].(float64)
	extIr, _ := payload["enableExternalIr"].(float64)

	var out []mediadCtl
	switch mode {
	case "manual":
		if level > 0 {
			out = append(out, mediadCtl{"nightvision", 1})
		} else {
			out = append(out, mediadCtl{"nightvision", 0})
		}
	case "auto":
		out = append(out, mediadCtl{"nightvision", 2})
		if switchMode == "lux" {
			out = append(out, mediadCtl{"night_lux", customValueToLux(custom)})
		}
		if extIr != 0 || level == 0 {
			out = append(out, mediadCtl{"ir_led", 0}) // filter only, no LED
		}
	}
	return out
}

// shutterControls maps videoMode to mediad's shutter enum (VI_SHUTTIME_MODE_E):
// default=Auto, sport=Frame Capture, slowShutter=Best Low Light.
func shutterControls(video map[string]interface{}) []mediadCtl {
	vm, ok := video["videoMode"].(string)
	if !ok {
		return nil
	}
	switch vm {
	case "default":
		return []mediadCtl{{"shutter", 0}}
	case "sport":
		return []mediadCtl{{"shutter", 1}}
	case "slowShutter":
		return []mediadCtl{{"shutter", 2}}
	}
	return nil
}

type mediadCtl struct {
	key   string
	value int
}

func ispControlMap(payload map[string]interface{}) []mediadCtl {
	var out []mediadCtl
	// Every mapped field is emitted here; suppression of unchanged fields is
	// done by mediadApplyControls' per-key delta tracking.
	add := func(field, key string, fn func(float64) int) {
		var v float64
		switch n := payload[field].(type) {
		case float64:
			v = n
		case int:
			v = float64(n)
		default:
			return
		}
		out = append(out, mediadCtl{key, fn(v)})
	}

	add("brightness", "brightness", func(v float64) int { return scaleLinear(v, 0, 100) })
	add("contrast", "contrast", func(v float64) int { return scaleLinear(v, 0, 100) })
	add("hue", "hue", func(v float64) int { return scaleLinear(v, 0, 100) })
	add("saturation", "saturation", func(v float64) int { return scaleLinear(v, 0, 100) })
	add("sharpness", "sharpness", func(v float64) int { return scaleLinear(v, 0, 10) })
	add("denoise", "denoise", func(v float64) int { return scaleLinear(v, 0, 100) })
	// 3DNR (tdf) is a module enable, not a 0-100 level; pinned OFF by default
	// (MEDIAD_3DNR=no) to avoid mediad's low-light temporal-filter ghosting.
	if cfg.Mediad3DNR {
		add("enable3dnr", "tdf", func(v float64) int { return scaleLinear(v, 0, 100) })
	} else {
		out = append(out, mediadCtl{"tdf", 0})
	}
	// HDR is a PLTM module enable plus a strength: wdr 1 (Protect default) is
	// stock pltm=1/wdr=0, 0 disables the module, 2/3 raise the strength.
	add("wdr", "pltm", func(v float64) int {
		if int(v) == 0 {
			return 0
		}
		return 1
	})
	add("wdr", "wdr", func(v float64) int {
		switch int(v) {
		case 2:
			return 128
		case 3:
			return 255
		default:
			return 0 // wdr=0 is gated by pltm=0; wdr=1 is the stock strength
		}
	})
	add("mirror", "mirror", func(v float64) int {
		if v != 0 {
			return 1
		}
		return 0
	})
	add("flip", "flip", func(v float64) int {
		if v != 0 {
			return 1
		}
		return 0
	})
	// Power-line frequency -> AW_MPI_ISP_SetFlicker raw [0:disable, 1:50Hz, 2:60Hz,
	// 3:auto]; Protect carries it in `aeMode`, the numeric `frequency` is fallback.
	if mode, ok := payload["aeMode"].(string); ok {
		out = append(out, mediadCtl{"flicker", aeModeToFlicker(mode)})
	} else {
		add("frequency", "flicker", func(v float64) int {
			switch int(v) {
			case 1:
				return 1 // 50 Hz
			case 2:
				return 2 // 60 Hz
			default:
				return 3 // auto
			}
		})
	}
	return out
}

// aeModeToFlicker maps Protect's aeMode onto AW_MPI_ISP_SetFlicker's raw range
// [0:disable, 1:50Hz, 2:60Hz, 3:auto]. auto is the mediad/stock default.
func aeModeToFlicker(mode string) int {
	switch mode {
	case "flick50":
		return 1
	case "flick60":
		return 2
	default:
		return 3 // auto
	}
}

// videoStreamBitrate extracts the bitrate Protect asked for on one video
// stream. ChangeVideoSettings carries bitRateCbrAvg (CBR) or bitRateVbrMax (VBR).
func videoStreamBitrate(v map[string]interface{}) int {
	for _, f := range []string{"bitRateCbrAvg", "bitRateVbrMax"} {
		if n, ok := v[f].(float64); ok && n > 0 {
			return int(n)
		}
	}
	return 0
}

// mediaBitrateMax is the ceiling the SoC can sustain with WDR+PLTM; matches
// MEDIAD_BITRATE_MAX in mediad's main.c.
const mediaBitrateMax = 3000000

func clampBitrate(b int) int {
	if b < 48000 {
		b = 48000
	}
	if b > mediaBitrateMax {
		b = mediaBitrateMax
	}
	return b
}

// osdSettingsResponse echoes the requested OSD settings back to the controller
// over this camera's defaults, so it reports what was actually applied (it
// used to hardcode date/logo on and the name shown).
func osdSettingsResponse(payload map[string]interface{}) map[string]interface{} {
	resp := map[string]interface{}{
		"enableOverlay": 1, "logoScale": 50, "overlayColorId": 0,
		"textScale": 50, "useCustomLogo": 0,
	}
	for k := range resp {
		if v, ok := payload[k]; ok {
			resp[k] = v
		}
	}
	for _, n := range []string{"_1", "_2", "_3", "_4"} {
		osd := map[string]interface{}{
			"enableDate": 1, "enableLogo": 1, "enableReportdStatsLevel": 0,
			"enableStreamerStatsLevel": 0, "tag": getDeviceName(),
		}
		if sub, ok := payload[n].(map[string]interface{}); ok {
			for k, v := range sub {
				osd[k] = v
			}
		}
		resp[n] = osd
	}
	return resp
}
