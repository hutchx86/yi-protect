// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Native POSIX-mqueue reader for rmm's motion broadcast, replacing an earlier
// `ipc_read -n N` subprocess approach that had two bugs: a long-lived ipc_read
// went silently deaf after a few read cycles, and a leaked orphan competed for
// messages (a POSIX mqueue delivers each message to only one receiver).
//
// Wire format/byte patterns come from the real upstream source
// (ipc_read.c/ipc_read.h). The /ipc_dispatch queue carries much more than
// motion traffic (ISP/AE telemetry, etc.), so an exact 16-byte match against
// IPC_MOTION_START/STOP is required; treating every message as motion gives
// constant false positives.
//
// mq_open/mq_timedreceive are variadic C functions cgo cannot call directly,
// so they are wrapped in the non-variadic shims below.
package main

/*
#include <mqueue.h>
#include <fcntl.h>
#include <time.h>
#include <errno.h>
#include <stdlib.h>

static mqd_t motion_mq_open(const char *name) {
    return mq_open(name, O_RDONLY);
}

// Returns bytes received, -1 on error/timeout (check errno via cgo's second
// return value), matching mq_receive's own return convention.
static ssize_t motion_mq_timedreceive(mqd_t mq, char *buf, size_t len, int timeout_sec) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_sec += timeout_sec;
    return mq_timedreceive(mq, buf, len, NULL, &ts);
}
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// motionQueueSlot is any slot other than 1 or 2 (reserved by convention);
// picked arbitrarily, no registration protocol exists.
const motionQueueSlot = 6

// The exact 16-byte IPC_MOTION_START/STOP messages, copied verbatim from
// ipc_read.h.
var (
	ipcMotionStart = []byte{0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x7c, 0x00, 0x7c, 0x00, 0x00, 0x00, 0x00, 0x00}
	ipcMotionStop  = []byte{0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x7d, 0x00, 0x7d, 0x00, 0x00, 0x00, 0x00, 0x00}
)

// motionMQMaxMsgSize matches IPC_MESSAGE_MAX_SIZE from the real headers.
const motionMQMaxMsgSize = 512

// motionMQReceiveTimeout bounds each receive so the read loop can check the
// done channel and the failsafe timer without indefinite blocking.
const motionMQReceiveTimeout = 5 * time.Second

// openMotionQueue opens the numbered dispatch slot read-only. ENOENT is common
// just after boot, before dispatch's ipc_multiplex.so preload has broadcast.
func openMotionQueue(slot int) (C.mqd_t, error) {
	name := C.CString(fmt.Sprintf("/ipc_dispatch_%d", slot))
	defer C.free(unsafe.Pointer(name))
	mq, err := C.motion_mq_open(name)
	if int(mq) < 0 {
		return mq, err
	}
	return mq, nil
}

// receiveMotionMessage waits up to motionMQReceiveTimeout for one message. A
// timeout returns (nil, nil) so callers can distinguish "nothing new" from a
// genuine queue failure.
func receiveMotionMessage(mq C.mqd_t) ([]byte, error) {
	buf := make([]byte, motionMQMaxMsgSize)
	n, err := C.motion_mq_timedreceive(mq, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), C.int(motionMQReceiveTimeout/time.Second))
	if int(n) < 0 {
		// cgo captures errno as a syscall.Errno here. ETIMEDOUT is the
		// routine "nothing happened" case.
		if errors.Is(err, syscall.ETIMEDOUT) {
			return nil, nil
		}
		return nil, err
	}
	return buf[:int(n)], nil
}

// startMotionWatcher runs for one WSS connection, reading motionQueueSlot via
// native POSIX mqueue calls and translating IPC_MOTION_START/STOP into
// EventAnalytics pushes. The caller closes done on disconnect/reconnect.
func (c *Client) startMotionWatcher(done <-chan struct{}) {
	st := &motionState{}
	go c.runMotionFailsafe(done, st)

	backoff := time.Second
	for {
		select {
		case <-done:
			return
		default:
		}
		err := c.runMotionWatcherOnce(done, st)
		if err == nil {
			return // done fired
		}
		log.Printf("motion watcher: %v", err)
		select {
		case <-done:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runMotionFailsafe forces a stop if no STOP arrives within
// motionMaxEventDuration, independent of the queue descriptor's lifecycle.
func (c *Client) runMotionFailsafe(done <-chan struct{}, st *motionState) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			st.mu.Lock()
			stuck := st.active && time.Since(st.startTS) > motionMaxEventDuration
			st.mu.Unlock()
			if stuck {
				log.Printf("motion: no stop received within %s, forcing one", motionMaxEventDuration)
				c.handleMotionStop(st)
			}
		}
	}
}

// runMotionWatcherOnce opens the queue once and reads until done fires or a
// real error occurs. A failure returns an error so the caller retries with
// backoff, covering the early-boot ENOENT window before dispatch's
// ipc_multiplex.so preload has broadcast.
func (c *Client) runMotionWatcherOnce(done <-chan struct{}, st *motionState) error {
	mq, err := openMotionQueue(motionQueueSlot)
	if err != nil {
		return fmt.Errorf("open queue slot %d: %w", motionQueueSlot, err)
	}
	log.Printf("motion watcher: opened /ipc_dispatch_%d natively", motionQueueSlot)
	defer C.mq_close(mq)

	for {
		select {
		case <-done:
			return nil
		default:
		}

		msg, err := receiveMotionMessage(mq)
		if err != nil {
			return fmt.Errorf("receive on slot %d: %w", motionQueueSlot, err)
		}
		if msg == nil {
			continue // timeout, nothing new -- loop and recheck done
		}

		switch {
		case bytes.Equal(msg, ipcMotionStart):
			c.handleMotionStart(st)
		case bytes.Equal(msg, ipcMotionStop):
			c.handleMotionStop(st)
		default:
			// Ambient ISP/AE telemetry shares this queue; ignore anything
			// that isn't one of the exact motion messages (matches
			// ipc_read.c).
		}
	}
}

// motionState tracks the current event for EventAnalytics start/stop pushes.
// Duplicate starts and unmatched stops are suppressed.
type motionState struct {
	mu      sync.Mutex
	eventID int
	active  bool
	startTS time.Time
}

const motionMaxEventDuration = 5 * time.Minute

func (c *Client) handleMotionStart(st *motionState) {
	st.mu.Lock()
	if st.active {
		st.mu.Unlock()
		return
	}
	st.active = true
	st.startTS = time.Now()
	eventID := st.eventID
	st.mu.Unlock()

	log.Printf("motion: start (eventId %d)", eventID)
	payload := map[string]interface{}{
		"clockBestMonotonic": int(c.uptime()),
		"clockBestWall":      0,
		"clockMonotonic":     int(c.uptime()),
		"clockStream":        int(c.uptime()),
		"clockStreamRate":    1000,
		"clockWall":          time.Now().UnixMilli(),
		"edgeType":           "start",
		"eventId":            eventID,
		"eventType":          "motion",
		"levels":             map[string]interface{}{"0": 47},
		"motionHeatmap":      "",
		"motionSnapshot":     "",
	}
	if err := c.send(c.genResponse("EventAnalytics", 0, payload)); err != nil {
		log.Printf("motion: failed to send start event: %v", err)
	}
}

func (c *Client) handleMotionStop(st *motionState) {
	st.mu.Lock()
	if !st.active {
		st.mu.Unlock()
		return
	}
	st.active = false
	startTS := st.startTS
	eventID := st.eventID
	st.eventID++
	st.mu.Unlock()

	log.Printf("motion: stop (eventId %d)", eventID)
	payload := map[string]interface{}{
		"clockBestMonotonic": int(c.uptime()),
		"clockBestWall":      startTS.UnixMilli(),
		"clockMonotonic":     int(c.uptime()),
		"clockStream":        int(c.uptime()),
		"clockStreamRate":    1000,
		"clockWall":          time.Now().UnixMilli(),
		"edgeType":           "stop",
		"eventId":            eventID,
		"eventType":          "motion",
		"levels":             map[string]interface{}{"0": 49},
		"motionHeatmap":      "heatmap.png",
		"motionSnapshot":     "motionsnap.jpg",
	}
	if err := c.send(c.genResponse("EventAnalytics", 0, payload)); err != nil {
		log.Printf("motion: failed to send stop event: %v", err)
	}
}
