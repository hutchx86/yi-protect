// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import "testing"

func TestNeedsStep(t *testing.T) {
	clockSynced.Store(false)
	pendingOffsetMs.Store(0)
	if !needsStep(13) {
		t.Fatal("first fix must step")
	}
	clockSynced.Store(true)

	// The 2026-09-24 sequence: one stale sample, then the true offset.
	if needsStep(-751) {
		t.Fatal("single out-of-tolerance sample must not step")
	}
	if needsStep(12) {
		t.Fatal("in-tolerance sample must not step")
	}
	if needsStep(-760) {
		t.Fatal("pending was cleared by the good sample; must wait again")
	}
	// Opposite-sign follow-up does not confirm.
	if needsStep(761) {
		t.Fatal("disagreeing samples must not step")
	}
	// Two agreeing samples do.
	if !needsStep(800) {
		t.Fatal("two agreeing samples must step")
	}
}
