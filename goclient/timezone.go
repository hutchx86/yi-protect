// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Controller timezone: Protect sends its zone as an IANA name in
// ChangeDeviceSettings ("Europe/Berlin"). The camera ships no zoneinfo and
// boots with TZ unset, so its OSD would otherwise run in UTC. Resolve the
// name here (embedded tzdata) into a POSIX TZ rule, which musl can use without
// zone files, and hand it to mediad's OSD.
package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"
)

// timezoneRawPath keeps the controller's own value so a restart can re-resolve
// it before the controller reconnects; timezonePosixPath is the resolved POSIX
// rule init.sh exports for the encoder at boot.
var (
	timezoneRawPath      = yipPrefix + "/etc/yi_protect_client_go.timezone"
	timezonePosixPath    = yipPrefix + "/etc/yi_protect_client_go.tz"
	timezoneOverridePath = yipPrefix + "/etc/yi_protect_client_go.timezone-override"
)

// controllerTimezone is the last value Protect sent (for the
// ChangeDeviceSettings ack); empty until it has sent one.
var controllerTimezone atomic.Value // holds string

// timezoneOverride is the per-camera web-UI override; when set it wins over the
// controller's value for the OSD.
var timezoneOverride atomic.Value // holds string

func currentTimezone() string {
	if v, _ := controllerTimezone.Load().(string); v != "" {
		return v
	}
	return "GMT0"
}

// timezoneState is what the web UI shows and edits.
type timezoneState struct {
	Controller string `json:"controller"` // last value Protect sent
	Override   string `json:"override"`   // per-camera override ("" = none)
	Effective  string `json:"effective"`  // override if set, else controller
	Applied    string `json:"applied"`    // POSIX rule currently handed to mediad
}

func timezoneStateNow() timezoneState {
	ctrl, _ := controllerTimezone.Load().(string)
	over, _ := timezoneOverride.Load().(string)
	eff := ctrl
	if over != "" {
		eff = over
	}
	tzAppliedMu.Lock()
	applied := tzApplied
	tzAppliedMu.Unlock()
	return timezoneState{Controller: ctrl, Override: over, Effective: eff, Applied: applied}
}

// effectiveTimezoneRaw is the value that should drive the OSD: the override if
// one is saved, else Protect's.
func effectiveTimezoneRaw() string {
	if v, _ := timezoneOverride.Load().(string); v != "" {
		return v
	}
	v, _ := controllerTimezone.Load().(string)
	return v
}

// applyEffectiveTimezone resolves and applies whichever source wins.
func applyEffectiveTimezone() {
	raw := effectiveTimezoneRaw()
	if raw == "" {
		return
	}
	posix, err := posixForZone(raw, time.Now())
	if err != nil {
		log.Printf("timezone: %v", err)
		return
	}
	applyPosix(posix)
}

// setTimezoneOverride saves the web UI's override (empty clears it) and applies
// it immediately. It validates the value before touching the card.
func setTimezoneOverride(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if _, err := posixForZone(raw, time.Now()); err != nil {
			return err
		}
	}
	if raw == "" {
		if err := os.Remove(timezoneOverridePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		timezoneOverride.Store("")
	} else {
		if err := os.WriteFile(timezoneOverridePath, []byte(raw+"\n"), 0644); err != nil {
			return err
		}
		timezoneOverride.Store(raw)
	}
	applyEffectiveTimezone()
	return nil
}

// posixTZRe matches a POSIX TZ rule with a mandatory std offset, e.g.
// "CET-1CEST,M3.5.0,M10.5.0/3", "PST8PDT,M3.2.0,M11.1.0" or "<+03>-3".
var posixTZRe = regexp.MustCompile(`^(?:[A-Za-z]{3,}|<[^<>]+>)[+-]?\d{1,2}(?::\d{1,2}){0,2}(?:[A-Za-z]{3,}|<[^<>]+>)?(?:[+-]?\d{1,2}(?::\d{1,2}){0,2})?(?:,.*)?$`)

// cachedLocation keeps the last LoadLocation result; the zone only changes when
// Protect sends a new one, and this runs on every timeSync beat.
var (
	tzLocMu   sync.Mutex
	tzLocName string
	tzLoc     *time.Location
	tzLocDone bool
)

func cachedLocation(name string) *time.Location {
	tzLocMu.Lock()
	defer tzLocMu.Unlock()
	if tzLocDone && name == tzLocName {
		return tzLoc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = nil
	}
	tzLocName, tzLoc, tzLocDone = name, loc, true
	return loc
}

// posixAbbr quotes an abbreviation POSIX cannot express bare (digits or signs,
// e.g. "<+03>").
func posixAbbr(name string) string {
	ok := len(name) >= 3
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			ok = false
			break
		}
	}
	if ok {
		return name
	}
	if name != "" && !strings.ContainsAny(name, "<>") {
		return "<" + name + ">"
	}
	return "UTC"
}

// posixOffset formats a Go east-positive offset as the POSIX west-positive form
// (UTC+1 -> "-1", UTC-5 -> "5", UTC+5:30 -> "-5:30").
func posixOffset(sec int) string {
	west := -sec
	sign := ""
	if west < 0 {
		sign = "-"
		west = -west
	}
	h, m, s := west/3600, (west%3600)/60, west%60
	switch {
	case s != 0:
		return fmt.Sprintf("%s%d:%02d:%02d", sign, h, m, s)
	case m != 0:
		return fmt.Sprintf("%s%d:%02d", sign, h, m)
	default:
		return fmt.Sprintf("%s%d", sign, h)
	}
}

// posixForZone turns a controller timezone into the POSIX rule musl needs. An
// IANA name is resolved at now's offset (re-applied on the timeSync beat, so a
// DST transition corrects itself); a value that is already a POSIX rule (older
// controllers) passes through.
func posixForZone(raw string, now time.Time) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 || strings.IndexFunc(raw, func(r rune) bool {
		return r < 0x20 || r > 0x7e
	}) >= 0 {
		return "", fmt.Errorf("invalid timezone %q", raw)
	}
	if loc := cachedLocation(raw); loc != nil {
		name, off := now.In(loc).Zone()
		return posixAbbr(name) + posixOffset(off), nil
	}
	if posixTZRe.MatchString(raw) {
		return raw, nil
	}
	return "", fmt.Errorf("unknown timezone %q", raw)
}

// tzAppliedMu guards the currently applied POSIX rule; the resolution can come
// from the controller goroutine and the timeSync beat at the same time.
var (
	tzAppliedMu sync.Mutex
	tzApplied   string
)

// applyPosix persists a resolved rule and pushes it to mediad's OSD. Re-applying
// the same rule is a no-op, so the 30 s timeSync beat is free.
func applyPosix(posix string) {
	tzAppliedMu.Lock()
	if posix == tzApplied {
		tzAppliedMu.Unlock()
		return
	}
	if err := os.WriteFile(timezonePosixPath, []byte(posix+"\n"), 0644); err != nil {
		log.Printf("timezone: persist %q failed: %v", posix, err)
	}
	// Our own children (ipc_cmd, snapshot) inherit it; Go's time.Local does not
	// reload, which is fine -- the client itself works in UTC.
	os.Setenv("TZ", posix)
	tzApplied = posix
	tzAppliedMu.Unlock()

	log.Printf("timezone: mediad TZ=%s", posix)
	go notifyMediadTimezone(posix)
}

func notifyMediadTimezone(posix string) {
	if !mediadEnabled() {
		return
	}
	reply, err := mediadCommand("tz " + posix)
	if err != nil {
		log.Printf("timezone: mediad tz %q failed: %v", posix, err)
		return
	}
	log.Printf("timezone: mediad tz %q -> %s", posix, reply)
}

// reapplyTimezone pushes the current rule to a freshly restarted mediad. It is
// a new process at its defaults, so applyPosix's identical-rule short-circuit
// must not hide it.
func reapplyTimezone() {
	tzAppliedMu.Lock()
	posix := tzApplied
	tzAppliedMu.Unlock()
	if posix != "" {
		notifyMediadTimezone(posix)
	}
}

// applyControllerTimezone takes Protect's ChangeDeviceSettings timezone and
// persists it. A saved web-UI override keeps winning for the OSD; the
// controller's own value is still stored and echoed in the ack.
func applyControllerTimezone(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if _, err := posixForZone(raw, time.Now()); err != nil {
		log.Printf("timezone: %v", err)
		return
	}
	controllerTimezone.Store(raw)
	if err := os.WriteFile(timezoneRawPath, []byte(raw+"\n"), 0644); err != nil {
		log.Printf("timezone: persist %q failed: %v", raw, err)
	}
	applyEffectiveTimezone()
}

// refreshTimezone re-resolves the effective zone; called on the timeSync beat
// so a DST transition inside a zone with no DST rule is picked up.
func refreshTimezone() {
	applyEffectiveTimezone()
}

// loadTimezone restores the override and controller zone from the previous run,
// before the controller reconnects (mediad has already started).
func loadTimezone() {
	if data, err := os.ReadFile(timezoneOverridePath); err == nil {
		raw := strings.TrimSpace(string(data))
		if raw != "" {
			if _, err := posixForZone(raw, time.Now()); err != nil {
				log.Printf("timezone: override ignored: %v", err)
			} else {
				timezoneOverride.Store(raw)
				log.Printf("timezone: override %q", raw)
			}
		}
	}
	if data, err := os.ReadFile(timezoneRawPath); err == nil {
		raw := strings.TrimSpace(string(data))
		if raw != "" {
			if _, err := posixForZone(raw, time.Now()); err != nil {
				log.Printf("timezone: persisted value: %v", err)
			} else {
				controllerTimezone.Store(raw)
			}
		}
	}
	applyEffectiveTimezone()
}
