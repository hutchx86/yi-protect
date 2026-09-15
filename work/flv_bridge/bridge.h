/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Shared types and constants for unifi_flv_bridge.
 *
 * The fshare shared-memory framing was reverse engineered; everything here
 * is our own code except that on-device format knowledge.
 */
#ifndef UNIFI_FLV_BRIDGE_BRIDGE_H
#define UNIFI_FLV_BRIDGE_BRIDGE_H

#include <pthread.h>

#include <atomic>
#include <cstdint>
#include <queue>
#include <vector>

// Stock rmm publishes video/audio into this POSIX shared-memory ring. The
// size is read from the /dev/shm file (BUFFER_FILE); shm_open() needs the
// bare name (BUFFER_SHM).
#define FSHARE_BUF_FILE "/dev/shm/fshare_frame_buf"
#define FSHARE_BUF_SHM "fshare_frame_buf"

// Backpressure bound on each per-channel queue.
#define MAX_QUEUE_SIZE 20

// Frame classification tags. The numeric values are historical (they were
// chosen to coincide with the resolution heights) and are only used
// internally, but keep them stable.
#define TYPE_NONE 0
#define TYPE_LOW 360
#define TYPE_HIGH 1080
#define TYPE_AAC 65521

// Which encoder outputs to forward.
#define RESOLUTION_NONE 0
#define RESOLUTION_LOW 360
#define RESOLUTION_HIGH 1080
#define RESOLUTION_BOTH 1440

// Supported cameras (the subset yi-hack/rmm knows about). Only the frame
// ring offset/header-size differ between them; see modelParams().
enum {
    Y20GA,
    Y25GA,
    Y30QA,
    Y501GC,
    Y21GA,
    Y211GA,
    Y211BA,
    Y213GA,
    Y291GA,
    H30GA,
    R30GB,
    R35GB,
    R37GB,
    R40GA,
    H51GA,
    H52GA,
    H60GA,
    Y28GA,
    Y29GA,
    Y623,
    Q321BR_LSX,
    QG311R,
    B091QP,
};

// Where the frame stream begins inside the ring (bytes), and how many bytes
// of frame header precede each payload. A value of 0 for either requests
// autodetection at startup (used by the R30GB/R35GB/R37GB family).
struct ModelParams {
    unsigned offset;
    int headerSize;
};

ModelParams modelParams(int model);

// Parses a model name (as passed with -m, and as found in yi-hack's
// model_suffix file), returning one of the enum values above. Unknown names
// fall back to Y21GA, matching the upstream tool's default.
int parseModel(const char *name);

// One encoded frame handed from the reader to FlvPush.
typedef struct {
    std::vector<unsigned char> frame;
    uint32_t time;
} output_frame;

// A bounded FIFO protected by a mutex.
typedef struct {
    std::queue<output_frame> frame_queue;
    pthread_mutex_t mutex;
} output_queue;

#endif  // UNIFI_FLV_BRIDGE_BRIDGE_H
