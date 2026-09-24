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

// Backpressure bound on each per-channel queue. Reverted from 6 to 20: at 6 the
// bridge dropped the oldest frames under Wi-Fi jitter to stay live, but dropping
// P/reference frames smears the picture until the next IDR (~2 s at GOP 40),
// which read as heavy blockiness in Protect even though the locally decoded ring
// stream was clean. Latency benefit not worth the artifacts. Briefly raised to
// 60 on 2026-09-22 to test whether push-thread stalls under y623's motion-
// triggered bitrate spikes were still overrunning 20; reverted same day (60's
// extra live-view latency wasn't worth it) before that theory was confirmed —
// the CABAC/entropy-mode fix in init.sh (FREECODEC_EXTRA) was the one that
// actually explained the blockiness. If drops at 20 turn out to still be a
// real, measured problem, revisit with evidence from the bridge log's
// "still waiting" / drop counters rather than raising this blind again.
// 2026-09-23: overflow no longer drops single frames; flvPushEnqueue flushes
// the backlog and resumes on the next keyframe (freeze, not smear), and logs
// "queue overflow" with a running drop count.
// 2026-09-24: 20 (1 s) overflowed on measured 1.5 s WiFi write stalls (81
// frames flushed: live-view freezes, recording gaps). Capture-time stamps
// make a drained backlog replay with correct timing, so size every queue by
// time: 2 s each (video 40 @ 20 fps, AAC 32 x 64 ms, Opus 100 x 20 ms).
#define MAX_QUEUE_SIZE 40
#define MAX_AAC_QUEUE_SIZE 32
#define MAX_OPUS_QUEUE_SIZE 100

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
    unsigned offset;       // ring control-header bytes; 0 => autodetect
    int headerSize;        // frame-header bytes; 0 => autodetect
    unsigned highWidth;    // HIGH-channel (video1) encoder width
    unsigned highHeight;   // HIGH-channel (video1) encoder height
    bool ptz;              // real motorized pan/tilt base
    unsigned highBitrate;  // HIGH-channel bitrate to declare (bps); 0 => default
};

// Looks `name` up in $UNIFI_MODEL_TABLE (default /tmp/sd/unifi/etc/model_table)
// and returns its row. A missing table or row returns the conservative
// defaults (368/28, 2304x1296, no PTZ, 2 Mbps) and warns on stderr -- never a
// silent guess.
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

#endif  // UNIFI_FLV_BRIDGE_BRIDGE_H
