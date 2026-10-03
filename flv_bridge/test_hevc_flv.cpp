// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors
// Byte-exact checks of the HEVC extendedFlv tags: these layouts were reverse
// engineered from a native G5 and ms silently refuses anything else.
#include <cstdio>
#include <vector>
#include "hevc_flv.h"

typedef std::vector<unsigned char> Buf;
static int fails;
#define CHECK(c) do { if (!(c)) { printf("FAIL %s:%d %s\n", __FILE__, __LINE__, #c); fails++; } } while (0)

int main() {
    const unsigned char nal[] = {0x26, 0x01, 0xaa, 0xbb, 0xcc};

    // NAL unit tag: 0x18 key / 0x28 inter, 0x01, 3-byte comp time, u32 length, NAL.
    Buf k = buildHevcNaluTag(nal, sizeof(nal), true);
    const unsigned char expK[] = {0x18, 0x01, 0, 0, 0, 0, 0, 0, 5, 0x26, 0x01, 0xaa, 0xbb, 0xcc};
    CHECK(k == Buf(expK, expK + sizeof(expK)));
    Buf p = buildHevcNaluTag(nal, sizeof(nal), false);
    CHECK(p.size() == k.size() && p[0] == 0x28 && p[1] == 0x01);

    // Codec config tag: 0x68 0x01, then u16-length-prefixed VPS, SPS, PPS.
    Buf vps = {0x40, 0x01}, sps = {0x42, 0x01, 0x02}, pps = {0x44};
    Buf c = buildHevcSequenceHeader(vps, sps, pps);
    const unsigned char expC[] = {0x68, 0x01, 0, 2, 0x40, 0x01, 0, 3, 0x42, 0x01, 0x02, 0, 1, 0x44};
    CHECK(c == Buf(expC, expC + sizeof(expC)));

    // NAL classification (HEVC type = bits 1..6 of byte 0).
    CHECK(hevcNalIsVps(0x40) && hevcNalIsSps(0x42) && hevcNalIsPps(0x44));
    CHECK(hevcNalIsIdr(0x26) && hevcNalIsIdr(0x28) && hevcNalIsIdr(0x2a));
    CHECK(!hevcNalIsIdr(0x02) && !hevcNalIsIdr(0x4e));

    printf(fails ? "test_hevc_flv: FAIL\n" : "test_hevc_flv: PASS\n");
    return fails ? 1 : 0;
}
