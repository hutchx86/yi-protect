/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * See FshareReader.h.
 */
#include "FshareReader.h"

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <ctime>
#include <vector>

#include <errno.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

namespace {

const unsigned kFrameOffsetTry1 = 300;
const unsigned kFrameOffsetTry2 = 368;

const unsigned char kPpsStart[] = {0x00, 0x00, 0x00, 0x01, 0x68};
const unsigned char kPpsMarker[] = {0x08, 0x00, 0x00, 0x00};

long long nowMs() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (long long)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

}  // namespace

unsigned char *FshareReader::movePtr(unsigned char *p, long delta) const {
    p += delta;
    if (delta > 0 && p > buffer_ + size_) p -= (size_ - offset_);
    if (delta < 0 && p < buffer_ + offset_) p += (size_ - offset_);
    return p;
}

void FshareReader::copyFromRing(unsigned char *dest, const unsigned char *src, size_t n) const {
    const unsigned char *ringEnd = buffer_ + size_;
    if (src + n > ringEnd) {
        size_t first = (size_t)(ringEnd - src);
        std::memcpy(dest, src, first);
        std::memcpy(dest + first, buffer_ + offset_, n - first);
    } else {
        std::memcpy(dest, src, n);
    }
}

FshareReader::FrameHeader FshareReader::readHeader(const unsigned char *p) const {
    unsigned char h[40];
    FrameHeader fh;
    size_t n = (size_t)headerSize_;
    if (n > sizeof(h)) n = sizeof(h);
    std::memset(h, 0, sizeof(h));
    std::memset(&fh, 0, sizeof(fh));
    copyFromRing(h, p, n);
    std::memcpy(&fh.len, h + 0, 4);
    std::memcpy(&fh.counter, h + 4, 4);
    if (headerSize_ == 22 || headerSize_ == 24) {
        std::memcpy(&fh.time, h + 12, 4);
        std::memcpy(&fh.type, h + 16, 2);
        std::memcpy(&fh.streamCounter, h + 18, 2);
    } else {
        // 26- and 28-byte headers share the same field placement.
        std::memcpy(&fh.time, h + 16, 4);
        std::memcpy(&fh.type, h + 20, 2);
        std::memcpy(&fh.streamCounter, h + 22, 2);
    }
    return fh;
}

unsigned FshareReader::autodetectOffset() const {
    uint32_t v = 0;
    std::memcpy(&v, buffer_ + kFrameOffsetTry1, sizeof(v));
    return v != 0 ? kFrameOffsetTry1 : kFrameOffsetTry2;
}

int FshareReader::autodetectHeaderSize() const {
    const unsigned char *ringStart = buffer_ + offset_;
    const unsigned char *ringEnd = buffer_ + size_;
    const unsigned char *pps = std::search(ringStart, ringEnd,
                                           kPpsStart, kPpsStart + sizeof(kPpsStart));
    if (pps == ringEnd) return 0;

    if ((size_t)(pps - buffer_) <= (size_t)offset_ + 40) return 0;
    const unsigned char *a1 = pps - 40;
    const unsigned char *marker = std::search(a1, pps,
                                              kPpsMarker, kPpsMarker + sizeof(kPpsMarker));
    if (marker == pps) return 0;

    long hs = (long)(pps - marker);
    if (hs < 0) hs += (long)(size_ - offset_);
    if (hs < 0 || hs > 40) return 0;
    return (int)hs;
}

void FshareReader::run(const Config &cfg, EmitFn emit, void *ctx) {
    int fshm = shm_open(cfg.shmName.c_str(), O_RDWR, 0);
    if (fshm == -1) {
        std::fprintf(stderr, "%lld: fshare: shm_open(%s) failed: %s\n",
                     nowMs(), cfg.shmName.c_str(), std::strerror(errno));
        return;
    }
    buffer_ = (unsigned char *)mmap(nullptr, cfg.size, PROT_READ | PROT_WRITE,
                                    MAP_SHARED, fshm, 0);
    close(fshm);
    if (buffer_ == MAP_FAILED) {
        buffer_ = nullptr;
        std::fprintf(stderr, "%lld: fshare: mmap failed: %s\n",
                     nowMs(), std::strerror(errno));
        return;
    }

    size_ = cfg.size;
    offset_ = cfg.offset;
    headerSize_ = cfg.headerSize;

    if (offset_ == 0) offset_ = autodetectOffset();

    // Autodetect the frame header size before touching the ring: the format
    // is only self-describing once we know where a frame begins.
    while (headerSize_ == 0) {
        headerSize_ = autodetectHeaderSize();
        if (headerSize_ == 0) {
            if (debug_) std::fprintf(stderr, "%lld: fshare: waiting for header-size marker\n", nowMs());
            usleep(10000);
        }
    }
    if (debug_) {
        std::fprintf(stderr, "%lld: fshare: offset=%u headerSize=%d ringSize=%zu\n",
                     nowMs(), offset_, headerSize_, size_);
    }

    // Seed the cursor at the current producer end. Everything already in the
    // ring when we attach is intentionally skipped.
    unsigned char *endPrev;
    {
        uint32_t start = 0, len = 0;
        std::memcpy(&start, buffer_ + 16, sizeof(start));
        std::memcpy(&len, buffer_ + 4, sizeof(len));
        endPrev = buffer_ + offset_ + start + len;
        if (endPrev >= buffer_ + size_) endPrev -= (size_ - offset_);
    }

    while (true) {
        uint32_t start = 0, len = 0, endOff = 0;
        std::memcpy(&start, buffer_ + 16, sizeof(start));
        std::memcpy(&len, buffer_ + 4, sizeof(len));

        unsigned char *end = buffer_ + offset_ + start + len;
        if (end >= buffer_ + size_) end -= (size_ - offset_);

        std::memcpy(&endOff, buffer_ + 12, sizeof(endOff));
        if (end != buffer_ + offset_ + endOff) {
            // Writer is mid-update; its fields don't agree yet.
            usleep(1000);
            continue;
        }
        if (end == endPrev) {
            usleep(10000);
            continue;
        }

        // Walk the newly written region, accumulating up to 10 frames. This
        // only computes frame boundaries; payload bytes are copied later.
        struct Pending {
            FrameHeader h;
            unsigned char *addr;
        } frames[10];
        unsigned char *cur = endPrev;
        int count = 0;
        bool sync = true;

        while (cur != end) {
            FrameHeader h = readHeader(cur);
            if (h.len > size_ - offset_ - (size_t)headerSize_) {
                sync = false;
                break;
            }
            frames[count].h = h;
            frames[count].addr = cur;
            cur = movePtr(cur, (long)h.len + headerSize_);
            count++;
            if (count == 10) {
                sync = false;
                break;
            }
        }

        if (!sync) {
            if (debug_ & 4) std::fprintf(stderr, "%lld: fshare: lost sync, resyncing\n", nowMs());
            endPrev = end;
            usleep(10000);
            continue;
        }

        // The final frame is retained for the next pass: the writer may not
        // have finished depositing it yet, so treating it as authoritative
        // risks emitting a truncated payload.
        if (count > 1) {
            endPrev = frames[count - 1].addr;
            count--;
        } else {
            usleep(10000);
            continue;
        }

        for (int i = 0; i < count; i++) {
            const FrameHeader &h = frames[i].h;
            unsigned char *payload = frames[i].addr;
            long plen = (long)h.len;

            int frameType;
            if (h.type & 0x0800) {
                frameType = TYPE_LOW;
            } else if (h.type & 0x0400) {
                frameType = TYPE_HIGH;
            } else if (h.type & 0x0100) {
                frameType = TYPE_AAC;
            } else {
                frameType = TYPE_NONE;
            }
            if (frameType == TYPE_NONE) continue;

            // SPS frames carry a 6-byte prefix ahead of the NAL in this
            // vendor framing; every other frame's payload starts right after
            // the header.
            if (h.type & 0x0002) {
                payload = movePtr(payload, headerSize_ + 6);
                plen -= 6;
            } else {
                payload = movePtr(payload, headerSize_);
            }
            if (plen <= 0) continue;

            // Copy out of the ring in one go so the consumer never sees a
            // torn write when the payload straddles the wrap point. The
            // vector is moved into the callback so ownership transfers
            // without another allocation/copy.
            std::vector<unsigned char> linear((size_t)plen);
            copyFromRing(linear.data(), payload, (size_t)plen);

            if (!emit(ctx, frameType, std::move(linear), h.counter, h.time, h.streamCounter)) {
                endPrev = end;
                munmap(buffer_, size_);
                buffer_ = nullptr;
                return;
            }
        }

        usleep(10000);
    }
}
