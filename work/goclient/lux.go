// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Lux-threshold "Custom" IR mode. The Protect UI's night-vision control is
// three-way: "Auto" (icrSwitchMode=="sensitivity", left to rmm's native
// day/night logic), "Custom" at either extreme (irLedMode=="manual", already
// wired in main.go), and "Custom" with a real 1-30 lux trigger threshold
// (irLedMode stays "auto", icrSwitchMode=="lux", icrCustomValue carries the
// slider -- this file).
//
// Brightness source: /dev/v4l-subdev0's Exposure/Gain V4L2 controls, which
// track real scene brightness. Reads the SENSOR SUBDEV, not /dev/videoN (held
// EBUSY by rmm); the control query is non-exclusive and coexists with rmm.
//
// No real lux calibration exists for this sensor, so this uses Gain as a
// brightness signal (far more dynamic range than Exposure in testing) and
// adapts to each camera's observed min/max over time, mapping the controller's
// 1-30 slider onto that range as a fraction. A relative brightness proxy, not
// calibrated lux.
package main

import (
	"log"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	aeSubdevPath  = "/dev/v4l-subdev0"
	v4l2CidGain   = 0x00980913
	vidiocGCtrlNr = 27
)

// vidiocGCtrl matches Linux's VIDIOC_G_CTRL: _IOWR('V', 27, struct v4l2_control).
var vidiocGCtrl = iowr('V', vidiocGCtrlNr, unsafe.Sizeof(v4l2Control{}))

func iowr(typ byte, nr byte, size uintptr) uintptr {
	const (
		iocRead  = 2
		iocWrite = 1
	)
	return (uintptr(iocRead|iocWrite) << 30) | (size << 16) | (uintptr(typ) << 8) | uintptr(nr)
}

type v4l2Control struct {
	ID    uint32
	Value int32
}

// readGain opens aeSubdevPath fresh each call (cheap, avoids holding a
// long-lived fd against a device other things may also want) and reads the
// live Gain control.
func readGain() (int32, error) {
	fd, err := syscall.Open(aeSubdevPath, syscall.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)

	ctrl := v4l2Control{ID: v4l2CidGain}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), vidiocGCtrl, uintptr(unsafe.Pointer(&ctrl)))
	if errno != 0 {
		return 0, os.NewSyscallError("ioctl VIDIOC_G_CTRL", errno)
	}
	return ctrl.Value, nil
}

// icrLuxState is the live config for the lux-threshold mode, set from
// handleIspSettings when a ChangeIspSettings carries icrSwitchMode=="lux".
// Guarded by icrLuxMu (read by the poller, written by the WSS handler).
var (
	icrLuxMu       sync.Mutex
	icrLuxEnabled  bool
	icrLuxSlider   int  // icrCustomValue, expected 1-30
	icrLuxExtOnly  bool // enableExternalIr: skip our own LED, filter only
	icrLuxNeedSync bool // set true whenever mode (re-)enables -- see below
	icrLuxPollOnce sync.Once
)

// setIcrLuxMode is called from handleIspSettings on every ChangeIspSettings
// message. Safe to call repeatedly with the same values (e.g. the slider
// firing a rapid sequence while being dragged, already observed live).
func setIcrLuxMode(enabled bool, slider int, extOnly bool) {
	icrLuxMu.Lock()
	changed := icrLuxEnabled != enabled || icrLuxSlider != slider || icrLuxExtOnly != extOnly
	// Force a resync on every disabled->enabled transition: the poller's own
	// isNight tracking starts at false regardless of the hardware's actual
	// state and only calls cpld_ctl on a CHANGE, so if its initial assumption
	// matches what it would decide, it silently never asserts anything. The
	// resync makes the next real decision assert to hardware once.
	if enabled && !icrLuxEnabled {
		icrLuxNeedSync = true
	}
	icrLuxEnabled = enabled
	icrLuxSlider = slider
	icrLuxExtOnly = extOnly
	icrLuxMu.Unlock()

	if changed {
		log.Printf("icr lux mode: enabled=%v slider=%d externalIrOnly=%v", enabled, slider, extOnly)
	}
	// Starts lazily, once, on first real use -- most cameras never engage
	// Custom-with-a-real-threshold.
	icrLuxPollOnce.Do(func() { go runIcrLuxPoller() })
}

// runIcrLuxPoller polls Gain every 5s, adapts its observed min/max, and drives
// cpld_ctl across the threshold with hysteresis + a debounce count, mirroring
// rmm's own >5-frame day/night debounce rather than reacting to one noisy
// reading.
func runIcrLuxPoller() {
	const (
		pollInterval   = 5 * time.Second
		debounceCount  = 3
		hysteresisFrac = 0.05 // +/-5% of the observed range around the trigger point
	)

	var (
		minGain, maxGain int32 = -1, -1
		isNight                = false
		crossCount             = 0
	)

	for {
		time.Sleep(pollInterval)

		icrLuxMu.Lock()
		enabled := icrLuxEnabled
		slider := icrLuxSlider
		extOnly := icrLuxExtOnly
		needSync := icrLuxNeedSync
		icrLuxMu.Unlock()

		if !enabled || slider < 1 || slider > 30 {
			crossCount = 0
			continue
		}

		gain, err := readGain()
		if err != nil {
			log.Printf("icr lux poller: readGain failed: %v", err)
			continue
		}

		if minGain < 0 || gain < minGain {
			minGain = gain
		}
		if gain > maxGain {
			maxGain = gain
		}
		if maxGain-minGain < 10 {
			// Not enough observed range yet to decide -- wait for more data
			// rather than guess off a near-zero range.
			continue
		}

		frac := float64(slider) / 30.0
		rng := float64(maxGain - minGain)
		trigger := float64(minGain) + frac*rng
		band := hysteresisFrac * rng

		var wantNight bool
		if isNight {
			wantNight = float64(gain) > trigger-band
		} else {
			wantNight = float64(gain) > trigger+band
		}

		if wantNight == isNight && !needSync {
			crossCount = 0
			continue
		}
		if !needSync {
			crossCount++
			if crossCount < debounceCount {
				continue
			}
		}
		// A resync applies immediately, skipping the debounce wait -- the
		// hardware may already disagree with our tracking (mode was just
		// (re-)enabled).
		crossCount = 0
		isNight = wantNight
		if needSync {
			icrLuxMu.Lock()
			icrLuxNeedSync = false
			icrLuxMu.Unlock()
			log.Printf("icr lux poller: resyncing hardware to computed state on mode (re-)enable")
		}

		if isNight {
			log.Printf("icr lux poller: gain=%d crossed trigger=%.0f (slider=%d, range=[%d,%d]) -> night", gain, trigger, slider, minGain, maxGain)
			runCpldCtl("ircut", "out")
			if !extOnly {
				runCpldCtl("led", "100")
			}
		} else {
			log.Printf("icr lux poller: gain=%d crossed trigger=%.0f (slider=%d, range=[%d,%d]) -> day", gain, trigger, slider, minGain, maxGain)
			if !extOnly {
				runCpldCtl("led", "0")
			}
			runCpldCtl("ircut", "in")
		}
	}
}
