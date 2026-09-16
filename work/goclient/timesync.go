// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors
//
// Camera clock sync, matching real hardware.
//
// The camera has no RTC; without this it boots to its firmware build date
// (2020 on this hardware) and stays there. Real hardware opens the control
// channel and sends ubnt_avclient_timeSync before hello, trading timestamps
// with the controller until its clock converges. This matters beyond logs:
// FlvPush's onClockSync tags carry the camera's wall clock and the controller
// maps every pushed frame through it -- a clock stuck years in the past gets
// the whole stream rejected, so live view shows nothing.
//
// The controller answers with a two-key NTP-style reply:
//
//	camera ->  ubnt_avclient_timeSync {"timeDelta": <offset>}
//	ctrl   <-  ubnt_avclient_timeSync {"t1": <ctrl wall ms>, "t2": <ctrl wall ms>}
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

// setSystemClock steps the wall clock to unixMs. We are root on the camera.
func setSystemClock(unixMs int64) error {
	tv := syscall.Timeval{
		Sec:  int32(unixMs / 1000),
		Usec: int32((unixMs % 1000) * 1000),
	}
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
// set) is applied and not answered; an unsolicited request is answered with
// our current offset.
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
		// Only step the clock on the first fix or a meaningful correction:
		// FlvPush derives tag timestamps from it, and stepping backwards
		// mid-connection would produce negative elapsed times.
		if !clockSynced.Load() || offset > 500 || offset < -500 {
			if err := setSystemClock(remote); err != nil {
				log.Printf("timeSync: settimeofday(%d) failed: %v", remote, err)
			} else {
				clockSynced.Store(true)
				log.Printf("timeSync: controller=%d local=%d offset=%dms -> system clock set",
					remote, local, offset)
			}
		}
	}

	if m.InResponseTo == 0 {
		// Controller-initiated request: answer with our offset.
		return false, c.send(c.genResponse("ubnt_avclient_timeSync", m.MessageID,
			map[string]interface{}{"timeDelta": currentTimeDeltaMs.Load()}))
	}
	return false, nil
}

// startTimeSyncLoop keeps asking the controller for its clock. On real
// hardware timeSync is the most frequent message and doubles as the camera's
// proof of life: a silent channel is eventually dropped, which makes Protect
// show the camera offline. Every 30s keeps the session warm and bounds drift.
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
