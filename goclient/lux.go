// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Lux-threshold "Custom" IR mode: arrives as irLedMode=="auto" + icrSwitchMode
// =="lux" + icrCustomValue. Reads /dev/v4l-subdev0 Gain as a brightness proxy.
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

// readGain opens aeSubdevPath fresh each call (avoids holding a long-lived fd
// against a device others may want) and reads the live Gain control.
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
var (
	icrLuxMu       sync.Mutex
	icrLuxEnabled  bool
	icrLuxSlider   int  // icrCustomValue, expected 1-30
	icrLuxExtOnly  bool // enableExternalIr: skip our own LED, filter only
	icrLuxNeedSync bool // set true whenever mode (re-)enables -- see below
	icrLuxPollOnce sync.Once
)

// setIcrLuxMode is called from handleIspSettings on every ChangeIspSettings;
// safe to call repeatedly with the same values (slider drags fire rapidly).
func setIcrLuxMode(enabled bool, slider int, extOnly bool) {
	icrLuxMu.Lock()
	changed := icrLuxEnabled != enabled || icrLuxSlider != slider || icrLuxExtOnly != extOnly
	// Force a resync on a disabled->enabled transition: the poller's isNight starts
	// false and only asserts on a CHANGE, so a matching start would stay silent.
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

// runIcrLuxPoller polls Gain every 5s, adapts its observed min/max and drives
// cpld_ctl with hysteresis + a debounce count (mirrors rmm's >5-frame debounce).
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
			// Not enough observed range yet -- wait rather than guess.
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
		// A resync applies immediately, skipping the debounce: the hardware
		// may already disagree with our tracking (mode was just re-enabled).
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
