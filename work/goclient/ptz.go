// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

// PTZ (pan/tilt/zoom) support for units with a real motorized pan/tilt base
// (see cfg.HasPTZ in main.go).
//
// The wire functionNames and the GetCurrentPosition response shape were
// extracted from the controller's own service.js, not guessed. The
// ContinuousMove and RelativePosition payloads were never confirmed live, so
// they're implemented as best-effort discrete direction moves (matching this
// hardware's ipc_cmd -m RIGHT/LEFT/UP/DOWN/STOP interface, which has no
// continuous-speed or absolute-coordinate concept). Raw payloads for every PTZ
// message are logged either way.

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ipcCmdPath = unifiPrefix + "/bin/ipc_cmd"

func ptzLogPayload(fn string, payload map[string]interface{}) {
	if raw, err := json.Marshal(payload); err == nil {
		log.Printf("%s raw payload: %s", fn, raw)
	}
}

// ptzGetPositionRegex matches ipc_cmd -g's own stdout, e.g.:
//
//	Current x  = 1563, y  = 809
//	Degrees x  = 137.2, y  = 63.5
//	137.2,63.5
var (
	ptzCurrentRegex = regexp.MustCompile(`Current x\s*=\s*(-?\d+),\s*y\s*=\s*(-?\d+)`)
	ptzDegreeRegex  = regexp.MustCompile(`Degrees x\s*=\s*(-?[\d.]+),\s*y\s*=\s*(-?[\d.]+)`)
)

// ptzGetPosition shells out to ipc_cmd -g and parses both raw step coordinates
// and degree-converted ones -- the firmware prints both, matching exactly what
// GetCurrentPosition's {inDegree:true,inSteps:true} request asks for.
func ptzGetPosition() (panSteps, tiltSteps int, panDeg, tiltDeg float64, err error) {
	out, err := exec.Command(ipcCmdPath, "-g").CombinedOutput()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if m := ptzCurrentRegex.FindStringSubmatch(string(out)); m != nil {
		panSteps, _ = strconv.Atoi(m[1])
		tiltSteps, _ = strconv.Atoi(m[2])
	}
	if m := ptzDegreeRegex.FindStringSubmatch(string(out)); m != nil {
		panDeg, _ = strconv.ParseFloat(m[1], 64)
		tiltDeg, _ = strconv.ParseFloat(m[2], 64)
	}
	return panSteps, tiltSteps, panDeg, tiltDeg, nil
}

// ptzMove sends one of ipc_cmd -m's discrete directions: RIGHT, LEFT, UP,
// DOWN, or STOP.
func ptzMove(dir string) {
	if err := exec.Command(ipcCmdPath, "-m", dir).Run(); err != nil {
		log.Printf("ipc_cmd -m %s failed: %v", dir, err)
	} else {
		log.Printf("ipc_cmd -m %s", dir)
	}
}

// ptzGotoPreset sends ipc_cmd -p N (slots 0-7; slot 0 is conventionally
// "home").
func ptzGotoPreset(slot int) {
	if err := exec.Command(ipcCmdPath, "-p", strconv.Itoa(slot)).Run(); err != nil {
		log.Printf("ipc_cmd -p %d failed: %v", slot, err)
	} else {
		log.Printf("ipc_cmd -p %d (goto preset)", slot)
	}
}

// ptzCruise sends ipc_cmd -C MODE ("on", "off", "presets", or "360").
func ptzCruise(mode string) {
	if err := exec.Command(ipcCmdPath, "-C", mode).Run(); err != nil {
		log.Printf("ipc_cmd -C %s failed: %v", mode, err)
	} else {
		log.Printf("ipc_cmd -C %s (cruise mode)", mode)
	}
}

// axisDirection turns a signed float (from an unverified controller-side
// coordinate scale) into one of this hardware's discrete directions, or ""
// for values close enough to zero to mean "no movement on this axis".
func axisDirection(v float64, positive, negative string) string {
	const deadband = 0.05
	switch {
	case v > deadband:
		return positive
	case v < -deadband:
		return negative
	default:
		return ""
	}
}

// handleContinuousMove implements CONTINUOUS_MOVE. Payload is the controller's
// {x,y,z} vector (field names from service.js; exact scale/sign not
// live-verified). x/y drive direction only (this hardware has no continuous
// speed); z (zoom) is ignored.
func (c *Client) handleContinuousMove(m Envelope) error {
	ptzLogPayload("ContinuousMove", m.Payload)
	x, _ := m.Payload["x"].(float64)
	y, _ := m.Payload["y"].(float64)

	// Prioritize whichever axis has larger magnitude -- this hardware can't
	// move both at once via -m.
	var dir string
	if x == 0 && y == 0 {
		dir = "STOP"
	} else if math.Abs(x) >= math.Abs(y) {
		dir = axisDirection(x, "RIGHT", "LEFT")
	} else {
		dir = axisDirection(y, "UP", "DOWN")
	}
	if dir != "" {
		ptzMove(dir)
	}
	if m.ResponseExpected {
		return c.send(c.genResponse("ContinuousMove", m.MessageID, nil))
	}
	return nil
}

// handleRelativePosition implements RELATIVE_POSITION. Payload names
// (panPos/tiltPos/panSpeed/tiltSpeed) are from service.js, but the coordinate
// scale was never confirmed against this hardware, so it is treated
// direction-only, like ContinuousMove, until live-tested.
func (c *Client) handleRelativePosition(m Envelope) error {
	ptzLogPayload("RelativePosition", m.Payload)
	panPos, _ := m.Payload["panPos"].(float64)
	tiltPos, _ := m.Payload["tiltPos"].(float64)

	var dir string
	if math.Abs(panPos) >= math.Abs(tiltPos) {
		dir = axisDirection(panPos, "RIGHT", "LEFT")
	} else {
		dir = axisDirection(tiltPos, "UP", "DOWN")
	}
	if dir != "" {
		go func() {
			ptzMove(dir)
			time.Sleep(300 * time.Millisecond)
			ptzMove("STOP")
		}()
	}
	if m.ResponseExpected {
		return c.send(c.genResponse("RelativePosition", m.MessageID, nil))
	}
	return nil
}

// ptzPresetsConfPath is the file the stock ptz_presets.sh bookkeeps
// ("N=name,x,y" per line, N 0-7). ipc_cmd -P itself can't report which slot it
// picked, so we reuse the script's own "first empty slot" prediction
// (edit_preset()) to report a real slot number to the controller.
const ptzPresetsConfPath = unifiPrefix + "/etc/ptz_presets.conf"

var ptzPresetLineRegex = regexp.MustCompile(`^(\d)=(.*)$`)

// ptzPresetSlots reads the 8 slot lines (0-7), returning slot->name ("" if
// empty). Missing/unparseable lines are treated as empty.
func ptzPresetSlots() [8]string {
	var slots [8]string
	data, err := os.ReadFile(ptzPresetsConfPath)
	if err != nil {
		return slots
	}
	for _, line := range strings.Split(string(data), "\n") {
		m := ptzPresetLineRegex.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		// value is "name,x,y" or just empty -- only the name matters here.
		name, _, _ := strings.Cut(m[2], ",")
		if n >= 0 && n <= 7 {
			slots[n] = name
		}
	}
	return slots
}

// ptzFirstAvailableSlot mirrors edit_preset()'s own algorithm in
// ptz_presets.sh: the lowest-numbered empty slot, or -1 if all 8 are used.
func ptzFirstAvailableSlot(slots [8]string) int {
	for i, name := range slots {
		if name == "" {
			return i
		}
	}
	return -1
}

// ptzWritePresetSlot rewrites ptzPresetsConfPath with one slot set/cleared,
// keeping our bookkeeping in sync with the stock script's file format.
func ptzWritePresetSlot(slots [8]string, slot int, value string) {
	slots[slot] = value
	content := ""
	for i, v := range slots {
		content += strconv.Itoa(i) + "=" + v + "\n"
	}
	if err := os.WriteFile(ptzPresetsConfPath, []byte(content), 0644); err != nil {
		log.Printf("ptzWritePresetSlot: failed to write %s: %v", ptzPresetsConfPath, err)
	}
}

// handlePtzPreset implements PRESET ("set"/"goto"/"delete"). Confirmed live
// shape for "set":
//
//	{"action":"set","item":{"focus":0,"index":-1,"name":"HomeView","pan":1563,"tilt":809,"zoom":0}}
//
// pan/tilt are raw step units (same as GetCurrentPosition's "steps"), not
// degrees. "goto"/"delete" were never observed live but are handled with
// best-fit field names. ipc_cmd -P can only save the camera's CURRENT position
// under a name, so "set" is only correct when the requested position is
// (approximately) where the camera already is -- true for the observed flow,
// which always calls GetCurrentPosition first.
func (c *Client) handlePtzPreset(m Envelope) error {
	ptzLogPayload("Preset", m.Payload)
	action, _ := m.Payload["action"].(string)
	item, _ := m.Payload["item"].(map[string]interface{})

	switch action {
	case "set":
		name, _ := item["name"].(string)
		if name == "" {
			name = "Preset"
		}
		index := -1
		if v, ok := item["index"].(float64); ok {
			index = int(v)
		}
		slots := ptzPresetSlots()
		slot := index
		if slot < 0 || slot > 7 || slots[slot] == "" {
			slot = ptzFirstAvailableSlot(slots)
		}
		if slot < 0 {
			log.Printf("Preset set %q: no free slot (all 8 in use)", name)
			break
		}
		if err := exec.Command(ipcCmdPath, "-P", name).Run(); err != nil {
			log.Printf("ipc_cmd -P %q failed: %v", name, err)
			break
		}
		log.Printf("ipc_cmd -P %q (assumed slot %d, first-available prediction)", name, slot)
		panSteps, tiltSteps, _, _, _ := ptzGetPosition()
		ptzWritePresetSlot(slots, slot, name+","+strconv.Itoa(panSteps)+","+strconv.Itoa(tiltSteps))
		if m.ResponseExpected {
			resp := map[string]interface{}{"item": map[string]interface{}{
				"focus": 0, "index": slot, "name": name,
				"pan": panSteps, "tilt": tiltSteps, "zoom": 0,
			}}
			return c.send(c.genResponse("Preset", m.MessageID, resp))
		}
		return nil
	case "delete", "remove":
		index := -1
		if v, ok := item["index"].(float64); ok {
			index = int(v)
		}
		if index < 0 || index > 7 {
			log.Printf("Preset delete: no usable index in payload, ignoring")
			break
		}
		if err := exec.Command(ipcCmdPath, "-R", strconv.Itoa(index)).Run(); err != nil {
			log.Printf("ipc_cmd -R %d failed: %v", index, err)
		} else {
			slots := ptzPresetSlots()
			ptzWritePresetSlot(slots, index, "")
			log.Printf("ipc_cmd -R %d (delete preset)", index)
		}
	case "goto":
		index := -1
		if v, ok := item["index"].(float64); ok {
			index = int(v)
		}
		if index >= 0 && index <= 7 {
			ptzGotoPreset(index)
		} else {
			log.Printf("Preset goto: no usable index in payload, ignoring")
		}
	default:
		log.Printf("Preset: unrecognized action %q, ignoring", action)
	}
	if m.ResponseExpected {
		return c.send(c.genResponse("Preset", m.MessageID, nil))
	}
	return nil
}

// handlePtzCenter implements PTZ_FUNCTION.CENTER ("Center") as a goto-home
// (preset slot 0 is conventionally "home" on this hardware).
func (c *Client) handlePtzCenter(m Envelope) error {
	ptzLogPayload("Center", m.Payload)
	ptzGotoPreset(0)
	if m.ResponseExpected {
		return c.send(c.genResponse("Center", m.MessageID, nil))
	}
	return nil
}

// handlePtzPatrol implements PATROL, mapped to this hardware's native cruise
// mode (ipc_cmd -C). Payload not live-captured -- looks for a boolean
// "enabled", defaulting to starting patrol.
func (c *Client) handlePtzPatrol(m Envelope) error {
	ptzLogPayload("Patrol", m.Payload)
	enabled := true
	if v, ok := m.Payload["enabled"].(bool); ok {
		enabled = v
	}
	if enabled {
		ptzCruise("presets")
	} else {
		ptzCruise("off")
	}
	if m.ResponseExpected {
		return c.send(c.genResponse("Patrol", m.MessageID, nil))
	}
	return nil
}

// handleGetCurrentPosition implements GET_CURRENT_POSITION. The controller
// sends it unprompted right after hello with {"inDegree":true,"inSteps":true}.
// The response shape ({degree:{pan,tilt,zoom},steps:{pan,tilt,zoom,focus}}) is
// the controller's own default. zoom/focus are always 0 -- no optical zoom.
func (c *Client) handleGetCurrentPosition(m Envelope) error {
	ptzLogPayload("GetCurrentPosition", m.Payload)
	panSteps, tiltSteps, panDeg, tiltDeg, err := ptzGetPosition()
	if err != nil {
		log.Printf("GetCurrentPosition: ipc_cmd -g failed: %v", err)
	}
	resp := map[string]interface{}{
		"degree": map[string]interface{}{"pan": panDeg, "tilt": tiltDeg, "zoom": 0},
		"steps":  map[string]interface{}{"pan": panSteps, "tilt": tiltSteps, "zoom": 0, "focus": 0},
	}
	return c.send(c.genResponse("GetCurrentPosition", m.MessageID, resp))
}
