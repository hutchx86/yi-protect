// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Camera clock sync: no RTC, so without this the camera boots to its build
// date and FlvPush's onClockSync tags get the whole stream rejected.
package main

import (
	"log"
	"sync/atomic"
	"syscall"
	"time"
)

// currentTimeDeltaMs is the last measured (controller - local) offset in ms.
// Sent back as the timeDelta field so the controller can see convergence.
var currentTimeDeltaMs atomic.Int64

// clockSynced records whether we have applied at least one correction.
var clockSynced atomic.Bool

// pendingOffsetMs holds an out-of-tolerance offset awaiting confirmation by
// the next sample (0 = none). Messages are handled inline, so a timeSync
// queued behind a slow handler reads stale: stepping on it moved the clock
// -751 ms, and the next sample stepped it back +761 ms.
var pendingOffsetMs atomic.Int64

// needsStep reports whether offset warrants a clock step: always for the
// first fix, else only when two consecutive samples agree within 200 ms.
func needsStep(offset int64) bool {
	if !clockSynced.Load() {
		return true
	}
	if offset <= 500 && offset >= -500 {
		pendingOffsetMs.Store(0)
		return false
	}
	prev := pendingOffsetMs.Swap(offset)
	if prev == 0 {
		return false
	}
	d := offset - prev
	return d <= 200 && d >= -200
}

// setSystemClock steps the wall clock to unixMs. We are root on the camera.
func setSystemClock(unixMs int64) error {
	tv := syscall.NsecToTimeval(unixMs * int64(time.Millisecond))
	return syscall.Settimeofday(&tv)
}

// sendTimeSync asks the controller "how wrong is my clock?" -- the camera's
// heartbeat on this channel, sent before hello and periodically thereafter.
func (c *Client) sendTimeSync() error {
	return c.send(Envelope{
		From:             "ubnt_avclient",
		To:               "UniFiVideo",
		FunctionName:     "ubnt_avclient_timeSync",
		MessageID:        c.nextID(),
		Payload:          map[string]interface{}{"timeDelta": currentTimeDeltaMs.Load()},
		ResponseExpected: true,
	})
}

// handleTimeSync applies a controller timeSync message. A reply (inResponseTo
// set) is applied and not answered; an unsolicited request is answered.
func (c *Client) handleTimeSync(m Envelope) (bool, error) {
	remote := int64(0)
	if v, ok := m.Payload["t1"].(float64); ok && v > 0 {
		remote = int64(v)
	} else if v, ok := m.Payload["t2"].(float64); ok && v > 0 {
		remote = int64(v)
	}

	if remote > 0 {
		local := time.Now().UnixMilli()
		offset := remote - local
		currentTimeDeltaMs.Store(offset)
		// Only step on the first fix or a confirmed meaningful correction.
		if needsStep(offset) {
			pendingOffsetMs.Store(0)
			if err := setSystemClock(remote); err != nil {
				log.Printf("timeSync: settimeofday(%d) failed: %v", remote, err)
			} else {
				clockSynced.Store(true)
				log.Printf("timeSync: controller=%d local=%d offset=%dms -> system clock set",
					remote, local, offset)
			}
		} else if offset > 500 || offset < -500 {
			log.Printf("timeSync: offset=%dms, waiting for a confirming sample", offset)
		}
	}

	if m.InResponseTo == 0 {
		// Controller-initiated request: answer with our offset.
		return false, c.send(c.genResponse("ubnt_avclient_timeSync", m.MessageID,
			map[string]interface{}{"timeDelta": currentTimeDeltaMs.Load()}))
	}
	return false, nil
}

// startTimeSyncLoop keeps asking the controller for its clock; on real hardware
// timeSync is the most frequent message and doubles as proof of life.
func (c *Client) startTimeSyncLoop(done <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := c.sendTimeSync(); err != nil {
				log.Printf("timeSync heartbeat failed: %v", err)
				return
			}
		}
	}
}
