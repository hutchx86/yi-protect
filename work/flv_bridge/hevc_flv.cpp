// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// H.265/HEVC video tags for UniFi's extendedFlv. See hevc_flv.h.
#include "hevc_flv.h"
#include "flvbytes.h"

// UniFi's extendedFlv marks the HEVC codec config with a video tag whose byte0
// is 0x68 and byte1 is 0x01, followed by u16-length-prefixed VPS, SPS and PPS
// (observed on a native G5's :7550 stream). It is NOT an ISO
// HEVCDecoderConfigurationRecord: ms does not recognise an hvcC here and
// reports videoCodec=VUNK with no recorder segments.
std::vector<unsigned char> buildHevcSequenceHeader(const std::vector<unsigned char> &vps,
                                                    const std::vector<unsigned char> &sps,
                                                    const std::vector<unsigned char> &pps) {
    std::vector<unsigned char> tag;
    put_u8(tag, 0x68); // frametype 6 (codec config) | codec 8 (HEVC)
    put_u8(tag, 0x01);
    auto add = [&](const std::vector<unsigned char> &n) {
        put_u16(tag, (uint16_t)n.size());
        put_bytes(tag, n.data(), n.size());
    };
    add(vps);
    add(sps);
    add(pps);
    return tag;
}

std::vector<unsigned char> buildHevcNaluTag(const unsigned char *nal, size_t len, bool isKey) {
    std::vector<unsigned char> tag;
    put_u8(tag, isKey ? 0x18 : 0x28); // frametype (1=key,2=inter), codecid=8 (HEVC)
    put_u8(tag, 0x01); // HEVCPacketType=1 (NALU)
    put_u24(tag, 0);   // composition time
    put_u32(tag, (uint32_t)len); // 4-byte length prefix
    put_bytes(tag, nal, len);
    return tag;
}
