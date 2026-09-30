// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Keyframe detection on an Annex-B access unit, H.264 and H.265. Header-only so
// the host test exercises the same code the push path runs.
#ifndef _KEYFRAME_H
#define _KEYFRAME_H

#include <cstddef>
#include <vector>

// H.264: SPS (7) or IDR (5) -> key; a non-IDR slice (1) ends the scan.
// H.265: NAL type is bits 1..6 of byte 0. VPS/SPS/PPS (32..34) or an IRAP
// (16..23) -> key; any VCL type below 16 ends the scan. (The H.264 masks can
// never match: HEVC byte 0 is type<<1, always even.)
inline bool annexBFrameIsKey(const unsigned char *buf, size_t n, bool hevc) {
    for (size_t i = 0; i + 3 < n; i++) {
        if (buf[i] != 0 || buf[i + 1] != 0 || buf[i + 2] != 1) continue;
        unsigned char hdr = buf[i + 3];
        if (hevc) {
            unsigned t = (hdr >> 1) & 0x3f;
            if ((t >= 32 && t <= 34) || (t >= 16 && t <= 23)) return true;
            if (t < 16) return false;
        } else {
            unsigned t = hdr & 0x1f;
            if (t == 7 || t == 5) return true;
            if (t == 1) return false;
        }
        i += 2;
    }
    return false;
}

inline bool annexBFrameIsKey(const std::vector<unsigned char> &buf, bool hevc) {
    return annexBFrameIsKey(buf.data(), buf.size(), hevc);
}

#endif
