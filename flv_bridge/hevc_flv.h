// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// H.265/HEVC in UniFi's extendedFlv: NAL classification and the two video tags
// (codec config, NAL unit). The H.264 equivalents stay in FlvPush.cpp; nothing
// here touches sockets or channel state.
#ifndef _HEVC_FLV_H
#define _HEVC_FLV_H

#include <cstddef>
#include <vector>

// HEVC NAL header: forbidden_zero(1) | nal_unit_type(6) | layer_id_hi(1).
inline unsigned hevcNalType(unsigned char hdr) { return (hdr >> 1) & 0x3f; }
inline bool hevcNalIsVps(unsigned char hdr) { return hevcNalType(hdr) == 32; }
inline bool hevcNalIsSps(unsigned char hdr) { return hevcNalType(hdr) == 33; }
inline bool hevcNalIsPps(unsigned char hdr) { return hevcNalType(hdr) == 34; }
inline bool hevcNalIsIdr(unsigned char hdr) {
    unsigned t = hevcNalType(hdr);
    return t >= 16 && t <= 23;   // BLA..CRA/IDR (IRAP)
}

// Codec config tag: byte0 0x68, byte1 0x01, then u16-length-prefixed VPS, SPS,
// PPS. NOT an ISO HEVCDecoderConfigurationRecord (see hevc_flv.cpp).
std::vector<unsigned char> buildHevcSequenceHeader(const std::vector<unsigned char> &vps,
                                                    const std::vector<unsigned char> &sps,
                                                    const std::vector<unsigned char> &pps);

// One NAL unit: 0x18 (key) / 0x28 (inter), 0x01, 3-byte composition time, then a
// 4-byte length prefix and the NAL (FLV codec id 8, not the standard 12).
std::vector<unsigned char> buildHevcNaluTag(const unsigned char *nal, size_t len, bool isKey);

#endif
