// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors
// Keyframe detection for both codecs. The H.264-only version returned false for
// every HEVC frame, so a discontinuity latched dropUntilKey forever (video gone,
// audio alive) until the bridge was restarted.
#include <cstdio>
#include <cstdlib>
#include <vector>
#include "keyframe.h"
#include "tsutil.h"

typedef std::vector<unsigned char> Buf;

static Buf au(std::initializer_list<unsigned char> nalHdrs) {
    Buf b;
    for (unsigned char h : nalHdrs) {
        const unsigned char sc[] = {0, 0, 0, 1};
        b.insert(b.end(), sc, sc + 4);
        b.push_back(h);
        b.push_back(0x01);   // second header byte / payload filler
        b.push_back(0xaa);
        b.push_back(0xbb);
    }
    return b;
}

static int fails;
#define CHECK(c) do { if (!(c)) { printf("FAIL %s:%d %s\n", __FILE__, __LINE__, #c); fails++; } } while (0)

int main() {
    // H.264: SPS 0x67, PPS 0x68, IDR 0x65, non-IDR 0x41
    CHECK( annexBFrameIsKey(au({0x67, 0x68, 0x65}), false));
    CHECK( annexBFrameIsKey(au({0x65}), false));
    CHECK(!annexBFrameIsKey(au({0x41}), false));
    // H.265: VPS 0x40, SPS 0x42, PPS 0x44, IDR_W_RADL 0x26, IDR_N_LP 0x28,
    // CRA 0x2a, TRAIL_R 0x02, TRAIL_N 0x00, SEI prefix 0x4e
    CHECK( annexBFrameIsKey(au({0x40, 0x42, 0x44, 0x26}), true));
    CHECK( annexBFrameIsKey(au({0x26}), true));
    CHECK( annexBFrameIsKey(au({0x28}), true));
    CHECK( annexBFrameIsKey(au({0x2a}), true));
    CHECK( annexBFrameIsKey(au({0x42}), true));
    CHECK( annexBFrameIsKey(au({0x4e, 0x26}), true));
    CHECK(!annexBFrameIsKey(au({0x02}), true));
    CHECK(!annexBFrameIsKey(au({0x00}), true));
    CHECK(!annexBFrameIsKey(au({0x4e, 0x02}), true));
    // A trailing IRAP after a P slice is not scanned (slice ends the scan).
    CHECK(!annexBFrameIsKey(au({0x02, 0x26}), true));
    CHECK(!annexBFrameIsKey(Buf(), true));
    // Audio timestamp guard: config tag at 86 ms, first frame captured at 58 ms.
    {
        uint32_t last = 0;
        CHECK(monotonicMs(last, 86) == 86);
        CHECK(monotonicMs(last, 58) == 86);   // was: 86 -> 58, ms drops the connection
        CHECK(monotonicMs(last, 86) == 86);
        CHECK(monotonicMs(last, 120) == 120);
        CHECK(last == 120);
        last = 0xFFFFFFF0u;                   // 32-bit wrap stays forward
        CHECK(monotonicMs(last, 5) == 5);
    }
    printf(fails ? "test_keyframe: FAIL\n" : "test_keyframe: PASS\n");
    return fails ? 1 : 0;
}
