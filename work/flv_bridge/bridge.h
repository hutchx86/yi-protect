/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Shared types and constants for unifi_flv_bridge. Everything here is our own
 * code except the reverse-engineered on-device fshare framing.
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

// Backpressure bound on each per-channel queue. Video arrives at ~20 fps, so
// 20 frames could buffer ~1 s of latency while the socket stalls; bound it far
// lower so the bridge drops to stay live instead of building a latency backlog.
// 6 frames ~= 300 ms at 20 fps (applies to the video, AAC and Opus queues).
#define MAX_QUEUE_SIZE 6

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

// Per-model facts, read from unifi/etc/model_table (the single per-model
// definition file; see work/sd_root/unifi/etc/model_table). The bridge never
// tests model names itself -- it asks for the row by name.
struct ModelParams {
    unsigned offset;      // ring control-header bytes; 0 => autodetect
    int headerSize;       // frame-header bytes; 0 => autodetect
    unsigned highWidth;   // HIGH-channel (video1) encoder width
    unsigned highHeight;  // HIGH-channel (video1) encoder height
    bool ptz;             // real motorized pan/tilt base
};

// Looks `name` up in $UNIFI_MODEL_TABLE (default /tmp/sd/unifi/etc/model_table)
// and returns its row. A missing table or row returns the conservative
// defaults (368/28, 2304x1296, no PTZ) and warns on stderr -- never a silent
// guess.
ModelParams modelParams(const char *name);

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
