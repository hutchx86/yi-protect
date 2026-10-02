// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// FLV audio timestamp guard. ms terminates the ingest ("Backward timestamp N->M
// on audio track") when the audio tag time steps back, including by 1 ms between
// the AAC and Opus tags, and a config tag (stamped "now") is followed by the first
// frame's older capture time. One guard covers both audio tracks of a connection.
#ifndef _TSUTIL_H
#define _TSUTIL_H

#include <cstdint>

// Returns ms, raised to `last` if it would step back; updates `last`.
inline uint32_t monotonicMs(uint32_t &last, uint32_t ms) {
    if ((int32_t)(ms - last) < 0) ms = last;
    last = ms;
    return ms;
}

#endif
