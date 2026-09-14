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
}

func mediadSet(key string, value int) error {
	reply, err := mediadCommand(fmt.Sprintf("set %s %d", key, value))
	if err != nil {
		mediadInvalidate()
		return err
	}
	if !strings.HasPrefix(reply, "ok ") {
		return fmt.Errorf("mediad: %s", reply)
	}
	return nil
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

// mediadApplyIspSettings forwards the picture-control subset of a
// ChangeIspSettings payload. Called off the websocket read loop; each request
// is a separate short-lived connection, so it is safe to interleave. No-op
// unless mediad is enabled and currently responsive.
func mediadApplyIspSettings(payload map[string]interface{}) {
	if !mediadEnabled() {
		return
	}
	for _, c := range ispControlMap(payload) {
		if err := mediadSet(c.key, c.value); err != nil {
			log.Printf("mediad ctl: %s=%d: %v", c.key, c.value, err)
		} else {
			log.Printf("mediad ctl: %s=%d ok", c.key, c.value)
		}
	}
}

type mediadCtl struct {
	key   string
	value int
}

func ispControlMap(payload map[string]interface{}) []mediadCtl {
	var out []mediadCtl
	// add forwards a field only when it actually differs from Protect's default
	// (ispSettingsDefaults). The controller repeats the full settings object on
	// every ChangeIspSettings, so without this the mapping would start
	// overriding at "default" values and change the stock look on connect.
	// Defaults are the stock replay's territory.
	add := func(field, key string, fn func(float64) int) {
		v, ok := payload[field].(float64)
		if !ok {
			return
		}
		if d, ok := defaultIspValue(field); ok && d == v {
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
	add("enable3dnr", "tdf", func(v float64) int { return scaleLinear(v, 0, 100) })
	// wdr is Off(0)/Med(1)/High(2), not 0-100; PLTM strength is 0-255.
	add("wdr", "wdr", func(v float64) int {
		switch int(v) {
		case 0:
			return 0
		case 2:
			return 255
		default:
			return 128
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
	// range is [0:disable, 1:50Hz, 2:60Hz, 3:auto]. Protect's field
	// name/encoding is not in ispSettingsDefaults(); handle a numeric
	// `frequency` of 0=auto/1=50/2=60 if it ever appears.
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
	return out
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

// defaultIspValue returns the numeric default for an ispSettingsDefaults field.
func defaultIspValue(field string) (float64, bool) {
	d, ok := ispSettingsDefaults()[field]
	if !ok {
		return 0, false
	}
	switch n := d.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
