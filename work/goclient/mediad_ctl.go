// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// mediad_ctl.go - optional client for the custom rmm replacement (mediad).
//
// When unifi.cfg sets IS_MEDIAD=yes AND the sister project's mediad is
// actually installed and running, this enables the "advanced" picture-control
// path: Protect's ChangeIspSettings picture fields are mapped onto mediad's
// vendor ISP setters and forwarded over mediad's unix control socket. Stock
// rmm cannot apply these controls, so without mediad the fields are simply
// acknowledged and ignored (the historical behavior).
//
// Detection is deliberately runtime, not just config: a deployment can carry
// IS_MEDIAD=yes against stock rmm (misconfiguration, or mediad crashed), so
// mediadEnabled() requires a running mediad process AND a responsive control
// socket before anything is forwarded. IR/night-vision remains on the
// existing cpld_ctl/lux path (mediad exposes no IR controls); only the
// picture controls and the HIGH encoder bitrate move to this socket.
//
// The wire protocol and the Protect-ui -> vendor-raw mappings were ported from
// yi-rmm-protect's prototype on 2026-09-14. yi-rmm-protect is now daemon-only
// (the rmm replacement itself); this client side is owned and extended here,
// gated behind IS_MEDIAD, with the daemon's control socket as the sole
// interface contract.
package main

import (
	"bufio"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mediadSockPath is mediad's control socket (MEDIAD_CTL_SOCK overrides, matching
// the daemon's own env handling).
func mediadSockPath() string {
	if p := os.Getenv("MEDIAD_CTL_SOCK"); p != "" {
		return p
	}
	return "/tmp/mediad_ctl.sock"
}

// mediadBinaryCandidates are the install locations a mediad build may land at.
// Only used to distinguish "IS_MEDIAD set but nothing installed" from "installed
// but not currently running" in the logs; a running mediad is detected from
// /proc regardless of where its binary lives.
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

// findMediadProcess scans /proc for a running mediad. It matches the process
// name (comm, i.e. the executable's basename) against a "mediad" prefix,
// excluding the "mediad_ctl" CLI tool. Returns the pid and the resolved exe
// path (empty if unreadable).
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

// mediadInstalled reports whether a mediad binary is present, and its path.
// A running mediad's own exe is authoritative; otherwise the known install
// locations are checked.
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

// mediad availability is cached briefly so a ChangeVideoSettings/bitrate burst
// doesn't rescan /proc per message. A failed send clears the cache so a killed
// mediad is noticed on the next call rather than after the TTL.
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

// mediadEnabled is the single gate for every advanced path. It is true only
// when IS_MEDIAD is set in unifi.cfg AND a mediad install is running with a
// responsive control socket. cfg.IsMediad is written once before run() starts
// and never mutated, so reading it here is race-free.
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
			// A (re)appeared daemon starts at its defaults; clear the delta
			// (without seeding) so the next settings object is applied in full.
			mediadDeltaReapply()
		} else {
			log.Printf("mediad: advanced controls DISABLED (installed=%v running=%v responsive=%v)", installed, running, responsive)
		}
	}
	mediadReady = ready
	return mediadReady
}

// logMediadStatus logs the startup state once, so an IS_MEDIAD=yes deployment
// against a missing/stopped mediad is visible in the log rather than silent.
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
}

// Some mediad controls are deliberately config/webui-only and are not settable
// over the socket (e.g. wdr/hdr on the current build: `err wdr is
// config/webui-only`). The daemon's advertised `list` is not reliable across
// builds, so instead of hardcoding which keys are locked we learn from the
// error and stop sending that key for the rest of the process lifetime. This
// keeps the client correct whether or not the daemon later unlocks a control.
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

// mediadDelta tracks, per socket key, the value last seen in a controller
// settings object, so a repeated object only re-issues the fields that actually
// changed. The controller resends the WHOLE ChangeIspSettings/
// ChangeVideoSettings object on every edit, so without this one slider move
// re-sends every non-default field (observed: brightness/contrast/saturation/
// nightvision/bitrate together) and the burst of IPC reconfigurations stalls
// mediad's ISP/VENC pipeline. `seed` marks the first object after a connect: it
// is recorded but not applied, so connecting doesn't burst either. `pending`
// forces one full apply on the next complete object, used when the daemon
// (re)appears -- a fresh mediad must receive the controller's settings, and
// that must survive the connect reset that follows.
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

// filter returns the subset of controls to forward, ending the seed once a
// complete object has been seen. While `pending`, partial objects are only
// recorded and an entire complete object is forwarded once. Caller holds
// mediadDeltaMu when shared.
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
// object); it deliberately leaves any `pending` daemon-appearance full apply in
// place. reset(false) is the re-arm after ResetIspSettings or a daemon
// (re)appearance: it forces a full apply on the next complete object. Caller
// holds mediadDeltaMu.
func (d *mediadDelta) reset(seed bool) {
	d.last = map[string]int{}
	d.seed = seed
	if !seed {
		d.pending = true
	}
}

var (
	mediadDeltaMu sync.Mutex
	// Separate deltas for the two message categories: the controller seeds each
	// independently on connect, so one must not clear the other's baseline.
	mediadIspDelta = newMediadDelta(true) // ChangeIspSettings (picture + night vision)
	mediadVidDelta = newMediadDelta(true) // ChangeVideoSettings (shutter + bitrate)
)

// resetMediadDelta drops the delta state and seeds the next object of each
// category. Called on every WSS connect (run()) so the controller's full
// resend is recorded but not re-applied. A pending daemon-appearance full apply
// is preserved.
func resetMediadDelta() {
	mediadDeltaMu.Lock()
	mediadIspDelta.reset(true)
	mediadVidDelta.reset(true)
	mediadDeltaMu.Unlock()
}

// mediadDeltaReapply forces a full apply on the next complete object. Used
// after ResetIspSettings (mediad is back at its defaults) and when a mediad
// daemon (re)appears.
func mediadDeltaReapply() {
	mediadDeltaMu.Lock()
	mediadIspDelta.reset(false)
	mediadVidDelta.reset(false)
	mediadDeltaMu.Unlock()
}

// mediadApplyControls forwards only the controls whose value changed since the
// previous object of the same category. `complete` marks the canonical full
// object; the seed is ended only then, so a partial object that arrives first
// (e.g. the legacy ChangeBrightnessSettings) is recorded without ending the
// seed -- otherwise the following full ChangeIspSettings would apply every
// field the partial object didn't mention. No-op unless mediad is enabled and
// responsive.
func mediadApplyControls(controls []mediadCtl, d *mediadDelta, complete bool) {
	if len(controls) == 0 || !mediadEnabled() {
		return
	}
	// Drop controls the daemon has reported as config/webui-only before the
	// delta sees them, so a locked key isn't recorded as applied and won't be
	// retried on the next object.
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

// mediadApplyIspSettings forwards the ChangeIspSettings subset: the picture
// controls plus the night-vision controls. When mediad is the producer it also
// owns the CPLD filter/LED and the day/night ISP tuning (via nightvision/
// night_lux/ir_led), so handleIspSettings skips its own cpld_ctl/lux path in
// that case; see calls.md in yi-rmm-protect. `complete` is false for the
// partial legacy ChangeBrightnessSettings object (see mediadApplyControls).
func mediadApplyIspSettings(payload map[string]interface{}, complete bool) {
	controls := ispControlMap(payload)
	controls = append(controls, nightVisionControls(payload)...)
	mediadApplyControls(controls, mediadIspDelta, complete)
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

// nightVisionControls maps Protect's night-vision fields to mediad's day/night
// controls. The controller sends these on every ChangeIspSettings:
//
//	irLedMode     "auto" | "manual"
//	irLedLevel    >0 = illuminator on; 0 with auto = "IR Filter Only"
//	icrSwitchMode "sensitivity" | "lux"
//	icrCustomValue 0-10, only meaningful with icrSwitchMode=="lux"
//	enableExternalIr 0/1, "IR Filter Only" (external illuminator)
//
// mediad exposes nightvision (0 always day / 1 always night / 2 auto),
// night_lux (auto switch threshold, lux) and ir_led (0-100). Always-On/Off map
// to nightvision; Auto/Custom map to nightvision=2 + night_lux. Filter-only
// states keep the LED off with a best-effort ir_led=0 after nightvision (the
// auto poller may re-assert it on a later switch -- a mediad gap, not handled
// here).
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

// shutterControls maps ChangeVideoSettings' videoMode to mediad's shutter enum
// (VI_SHUTTIME_MODE_E): default=Auto, sport=Frame Capture, slowShutter=Best Low
// Light (see calls.md). Unknown modes leave the current setting untouched.
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
	// Every mapped field is emitted here; suppression of unchanged fields (and
	// of the whole first object after a connect) is done by mediadApplyControls'
	// per-key delta tracking, not by comparing to static defaults.
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
	// tdf is a 0/1 module enable, not a 0-100 level; 1 == vendor stock. Protect
	// sends enable3dnr as 0/1, so the linear scale is the identity here.
	add("enable3dnr", "tdf", func(v float64) int { return scaleLinear(v, 0, 100) })
	// wdr wire values are 0..3. The camera's HDR is a PLTM module enable plus a
	// strength: stock (Protect's exposed default wdr=1) is pltm=1 + wdr=0, i.e. the
	// vendor tuning's own strength. Off (0) disables the module; 2/3 raise the
	// strength (values tentative, pending on-camera calibration).
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
	// Power-line frequency Auto/50/60 -> AW_MPI_ISP_SetFlicker, whose raw
	// range is [0:disable, 1:50Hz, 2:60Hz, 3:auto]. Protect carries the
	// power-line setting in `aeMode` ("auto"/"flick50"/"flick60"); auto maps to
	// the stock default. The numeric `frequency` field (0=auto/1=50/2=60) is
	// kept for a raw field if it ever appears.
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
	if mode, ok := payload["aeMode"].(string); ok {
		out = append(out, mediadCtl{"flicker", aeModeToFlicker(mode)})
	}
	return out
}

// aeModeToFlicker maps Protect's aeMode power-line setting onto
// AW_MPI_ISP_SetFlicker's raw range [0:disable, 1:50Hz, 2:60Hz, 3:auto].
// auto is the mediad/stock default.
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
// stream. ChangeVideoSettings carries it as bitRateCbrAvg (CBR) or
// bitRateVbrMax (VBR); prefer the CBR average when present.
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
