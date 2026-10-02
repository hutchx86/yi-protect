// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Host-side test for FshareReader's ring parsing (linear + wrap). Not part of
// the device build. Run with: make test
#include <cassert>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <thread>
#include <vector>

#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

#include "FshareReader.h"

static const size_t kRing = 65536;
static const unsigned kOffset = 368;
static const int kHdr = 28;

static void put32(unsigned char *p, uint32_t v) { std::memcpy(p, &v, 4); }
static void put16(unsigned char *p, uint16_t v) { std::memcpy(p, &v, 2); }

// Writes into the ring, wrapping at buffer+size back to buffer+offset.
static void ringWrite(unsigned char *base, size_t ringLen, size_t pos,
                      const unsigned char *data, size_t n) {
    for (size_t i = 0; i < n; i++) {
        size_t p = pos + i;
        while (p >= kRing) p -= ringLen;
        base[p] = data[i];
    }
}

static void frameBytes(std::vector<unsigned char> &out, uint16_t type,
                       const std::vector<unsigned char> &payload, uint16_t sc,
                       bool spsPrefix) {
    out.assign(kHdr + (spsPrefix ? 6 : 0) + payload.size(), 0);
    uint32_t len = (uint32_t)(payload.size() + (spsPrefix ? 6 : 0));
    put32(out.data() + 0, len);
    put32(out.data() + 4, 7);
    put32(out.data() + 16, 1234);
    put16(out.data() + 20, type);
    put16(out.data() + 22, sc);
    std::memcpy(out.data() + kHdr + (spsPrefix ? 6 : 0), payload.data(), payload.size());
}

struct Seen { int type; std::vector<unsigned char> data; };
static std::vector<Seen> g_seen;

static bool emit(void *, int frameType, std::vector<unsigned char> &&payload,
                 uint32_t, uint16_t sc) {
    (void)sc;
    g_seen.push_back({frameType, std::move(payload)});
    return g_seen.size() < 4;
}

static int runScenario(size_t start) {
    g_seen.clear();
    shm_unlink("/fsreader_test");
    int fd = shm_open("/fsreader_test", O_CREAT | O_RDWR, 0600);
    assert(fd >= 0);
    assert(ftruncate(fd, kRing) == 0);
    unsigned char *base = (unsigned char *)mmap(nullptr, kRing, PROT_READ | PROT_WRITE,
                                                MAP_SHARED, fd, 0);
    assert(base != MAP_FAILED);
    std::memset(base, 0, kRing);

    const size_t ringLen = kRing - kOffset;
    std::vector<unsigned char> sps = {1, 2, 3, 4, 5};
    std::vector<unsigned char> high = {0xAA, 0xBB, 0xCC};
    std::vector<unsigned char> low = {0x11, 0x22};
    std::vector<unsigned char> aac = {0x07, 0x08, 0x09, 0x0A};

    std::vector<unsigned char> f;
    size_t pos = kOffset + start;
    size_t total = 0;
    auto wf = [&](uint16_t type, const std::vector<unsigned char> &payload,
                  uint16_t sc, bool spsPrefix) {
        frameBytes(f, type, payload, sc, spsPrefix);
        ringWrite(base, kRing, pos, f.data(), f.size());
        pos += f.size();
        while (pos >= kRing) pos -= ringLen;
        total += f.size();
    };
    wf(0x0402, sps, 3, true);
    wf(0x0400, high, 4, false);
    wf(0x0800, low, 5, false);
    wf(0x0100, aac, 6, false);
    wf(0x0400, {0x99}, 7, false);  // trailing frame, retained by the reader

    size_t len = total;
    put32(base + 16, (uint32_t)start);
    put32(base + 4, 0);
    put32(base + 12, (uint32_t)start);

    FshareReader reader(0);
    FshareReader::Config cfg;
    cfg.shmName = "/fsreader_test";
    cfg.size = kRing;
    cfg.offset = kOffset;
    cfg.headerSize = kHdr;

    std::thread t([&]() { reader.run(cfg, emit, nullptr); });
    usleep(200000);
    put32(base + 4, (uint32_t)len);
    put32(base + 12, (uint32_t)((start + len) % ringLen));
    t.join();

    int rc = 0;
    if (g_seen.size() != 4) {
        std::fprintf(stderr, "FAIL(start=%zu): expected 4 frames, got %zu\n", start, g_seen.size());
        rc = 1;
    } else {
        auto check = [&](int i, int wantType, const std::vector<unsigned char> &want) {
            if (g_seen[i].type != wantType || g_seen[i].data != want) {
                std::fprintf(stderr, "FAIL(start=%zu) frame %d mismatch\n", start, i);
                rc = 1;
            }
        };
        check(0, TYPE_HIGH, sps);
        check(1, TYPE_HIGH, high);
        check(2, TYPE_LOW, low);
        check(3, TYPE_AAC, aac);
    }
    munmap(base, kRing);
    close(fd);
    shm_unlink("/fsreader_test");
    return rc;
}

// A model absent from the table (or an empty/unknown name) must fall back to
// ring autodetection, not a hardcoded offset/header.
static int testModelFallback() {
    setenv("UNIFI_MODEL_TABLE", "/nonexistent/model_table", 1);
    ModelParams mp = modelParams("no-such-model");
    ModelParams empty = modelParams("");
    unsetenv("UNIFI_MODEL_TABLE");
    if (mp.offset != 0 || mp.headerSize != 0 || empty.offset != 0 || empty.headerSize != 0) {
        std::fprintf(stderr, "FAIL: unknown-model fallback should autodetect ring "
                             "(got offset=%u/%u header=%d/%d)\n",
                     mp.offset, empty.offset, mp.headerSize, empty.headerSize);
        return 1;
    }
    return 0;
}

// The bridge must declare the geometry of the ACTIVE encoder: the model row's
// mediad_w/h when UNIFI_ENCODER=mediad, else the stock-rmm high_w/h (and
// high_w/h when a row has no mediad columns).
static int testModelGeometry() {
    const char *path = "/tmp/fsreader_model_table";
    FILE *f = std::fopen(path, "w");
    if (f == nullptr) return 1;
    std::fprintf(f, "# test\n");
    std::fprintf(f, "h51ga gc2053 368 28 2304 1296 yes 0 1920 1080\n");
    std::fprintf(f, "h52ga gc2053 368 28 1920 1080 yes\n");  // no mediad columns
    std::fclose(f);
    setenv("UNIFI_MODEL_TABLE", path, 1);

    setenv("UNIFI_ENCODER", "rmm", 1);
    ModelParams r = modelParams("h51ga");
    setenv("UNIFI_ENCODER", "mediad", 1);
    ModelParams m = modelParams("h51ga");
    ModelParams mFallback = modelParams("h52ga");  // mediad path, no columns
    unsetenv("UNIFI_ENCODER");
    ModelParams d = modelParams("h51ga");          // unset -> stock geometry
    unsetenv("UNIFI_MODEL_TABLE");
    std::remove(path);

    int rc = 0;
    if (r.highWidth != 2304 || r.highHeight != 1296) {
        std::fprintf(stderr, "FAIL: rmm h51ga geometry %ux%u, want 2304x1296\n",
                     r.highWidth, r.highHeight);
        rc = 1;
    }
    if (m.highWidth != 1920 || m.highHeight != 1080) {
        std::fprintf(stderr, "FAIL: mediad h51ga geometry %ux%u, want 1920x1080\n",
                     m.highWidth, m.highHeight);
        rc = 1;
    }
    if (mFallback.highWidth != 1920 || mFallback.highHeight != 1080) {
        std::fprintf(stderr, "FAIL: mediad h52ga fallback %ux%u, want 1920x1080\n",
                     mFallback.highWidth, mFallback.highHeight);
        rc = 1;
    }
    if (d.highWidth != 2304 || d.highHeight != 1296) {
        std::fprintf(stderr, "FAIL: unset encoder h51ga geometry %ux%u, want 2304x1296\n",
                     d.highWidth, d.highHeight);
        rc = 1;
    }
    return rc;
}

int main() {
    int rc = 0;
    rc |= runScenario(10);              // fully linear
    rc |= runScenario(kRing - kOffset - 100);  // straddles the wrap point
    rc |= testModelFallback();
    rc |= testModelGeometry();
    if (rc == 0) std::printf("PASS: linear + wrap scenarios + model fallback + geometry\n");
    return rc;
}
