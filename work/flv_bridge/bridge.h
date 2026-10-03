/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Shared types and constants for yi_protect_flv_bridge. Everything here is our own
 * code except the reverse-engineered on-device fshare framing.
 */
#ifndef YI_PROTECT_FLV_BRIDGE_BRIDGE_H
#define YI_PROTECT_FLV_BRIDGE_BRIDGE_H

#include <pthread.h>

#include <atomic>
#include <cstdint>
#include <queue>
#include <vector>

// Stock rmm's shared-memory ring: size from the /dev/shm file, shm_open() takes
// the bare name.
#define FSHARE_BUF_FILE "/dev/shm/fshare_frame_buf"
#define FSHARE_BUF_SHM "fshare_frame_buf"

// Per-channel queues hold ~2 s (video 40 @ 20 fps, AAC 32 x 64 ms, Opus 100 x 20 ms);
// on video overflow the backlog is flushed and resumes at the next keyframe.
#define MAX_QUEUE_SIZE 40
#define MAX_AAC_QUEUE_SIZE 32
#define MAX_OPUS_QUEUE_SIZE 100

// Frame classification tags; internal only, values match resolution heights.
#define TYPE_NONE 0
#define TYPE_LOW 360
#define TYPE_HIGH 1080
#define TYPE_AAC 65521

// Which encoder outputs to forward.
#define RESOLUTION_NONE 0
#define RESOLUTION_LOW 360
#define RESOLUTION_HIGH 1080
#define RESOLUTION_BOTH 1440

// Per-model facts from yi-protect/etc/model_table; the bridge never tests model
// names itself.
struct ModelParams {
    unsigned offset;       // ring control-header bytes; 0 => autodetect
    int headerSize;        // frame-header bytes; 0 => autodetect
    unsigned highWidth;    // HIGH-channel (video1) encoder width
    unsigned highHeight;   // HIGH-channel (video1) encoder height
    bool ptz;              // real motorized pan/tilt base
    unsigned highBitrate;  // HIGH-channel bitrate to declare (bps); 0 => default
};

// Row for `name` from $YIP_MODEL_TABLE (default /tmp/sd/yi-protect/etc/model_table);
// missing table/row: warn and use defaults (368/28, 2304x1296, no PTZ, 2 Mbps).
ModelParams modelParams(const char *name);

// One encoded frame handed from the reader to FlvPush.
typedef struct {
    std::vector<unsigned char> frame;
    uint32_t time = 0; // capture PTS, monotonic ms (mediad ring header)
} output_frame;

// A bounded FIFO protected by a mutex.
typedef struct {
    std::queue<output_frame> frame_queue;
    pthread_mutex_t mutex;
} output_queue;

#endif  // YI_PROTECT_FLV_BRIDGE_BRIDGE_H
