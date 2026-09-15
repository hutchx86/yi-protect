/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * FshareReader: reads the stock video encoder's ("rmm") POSIX shared-memory
 * frame ring at /dev/shm/fshare_frame_buf and emits each encoded frame.
 *
 * Reimplements the stock encoder's buffer-walking logic (LIVE555/RTSP removed).
 * The ring layout -- header fields, per-model offsets, frame tags -- was
 * reverse engineered.
 */
#ifndef UNIFI_FLV_BRIDGE_HSHARE_READER_H
#define UNIFI_FLV_BRIDGE_HSHARE_READER_H

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "bridge.h"

class FshareReader {
public:
    struct Config {
        std::string shmName = FSHARE_BUF_SHM;
        size_t size = 0;      // ring size in bytes
        unsigned offset = 0;  // 0 => autodetect
        int headerSize = 0;   // 0 => autodetect
    };

    // Called once per fully parsed frame. `payload` is the frame's bytes with
    // the frame header (and, for SPS frames, the extra 6-byte prefix the
    // vendor framing uses) already skipped, linearized out of the ring and
    // moved in so the consumer can take ownership without a second copy.
    // Return false to stop the reader.
    typedef bool (*EmitFn)(void *ctx, int frameType,
                           std::vector<unsigned char> &&payload,
                           uint32_t time, uint16_t streamCounter);

    explicit FshareReader(int debug) : debug_(debug) {}

    // Maps the ring and reads it forever. Returns only if `emit` returns
    // false or a fatal setup error occurs.
    void run(const Config &cfg, EmitFn emit, void *ctx);

private:
    struct FrameHeader {
        uint32_t len;
        uint32_t time;
        uint16_t type;
        uint16_t streamCounter;
    };

    // Circular-buffer primitives. The logical ring occupies
    // [buffer_ + offset_, buffer_ + size_).
    unsigned char *movePtr(unsigned char *p, long delta) const;
    void copyFromRing(unsigned char *dest, const unsigned char *src, size_t n) const;
    FrameHeader readHeader(const unsigned char *p) const;

    unsigned autodetectOffset() const;
    int autodetectHeaderSize() const;

    int debug_;
    unsigned char *buffer_ = nullptr;
    size_t size_ = 0;
    unsigned offset_ = 0;
    int headerSize_ = 0;
};

#endif  // UNIFI_FLV_BRIDGE_HSHARE_READER_H
