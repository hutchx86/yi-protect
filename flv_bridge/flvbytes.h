// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Big-endian byte writers shared by the FLV muxer (FlvPush.cpp) and the HEVC tag
// builders (hevc_flv.cpp).
#ifndef _FLVBYTES_H
#define _FLVBYTES_H

#include <cstddef>
#include <cstdint>
#include <vector>

inline void put_u8(std::vector<unsigned char> &b, uint8_t v) { b.push_back(v); }
inline void put_u16(std::vector<unsigned char> &b, uint16_t v) {
    b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
inline void put_u24(std::vector<unsigned char> &b, uint32_t v) {
    b.push_back((v >> 16) & 0xff); b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
inline void put_u32(std::vector<unsigned char> &b, uint32_t v) {
    b.push_back((v >> 24) & 0xff); b.push_back((v >> 16) & 0xff);
    b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
inline void put_bytes(std::vector<unsigned char> &b, const unsigned char *p, size_t n) {
    b.insert(b.end(), p, p + n);
}

#endif
