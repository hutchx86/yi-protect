// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// FlvPush: muxes the raw H.264/AAC frames from the fshare ring into extendedFlv
// and pushes them over TCP. Channels HIGH=video1, LOW=video2, MED=video3 (aliases
// the LOW encoder output). Control FIFO commands: see docs/flv-bridge.md.
#ifndef _FLV_PUSH_H
#define _FLV_PUSH_H

#include "bridge.h"

#define FLV_PUSH_FIFO "/tmp/unifi_flv_bridge_ctl"

enum { FLV_CH_HIGH = 0, FLV_CH_LOW = 1, FLV_CH_MED = 2, FLV_CH_COUNT = 3 };

// Sets the HIGH-channel encoder geometry from the model table. Must be called
// before flvPushInit(); values <= 0 are ignored (compiled default retained).
void flvPushSetHighResolution(unsigned width, unsigned height);

// Sets the HIGH-channel declared bitrate (bps) from the model table. Must be
// called before flvPushInit(); 0 is ignored (compiled default retained).
void flvPushSetHighBandwidth(unsigned bps);

// Called once from main(): create the control FIFO (if missing) and start its
// reader thread.
void flvPushInit();

// True iff this channel's push destination is connected. Gates per-frame
// duplication so an idle channel costs nothing.
bool flvPushActive(int channel);

// Enqueue one parsed video frame for its channel; cheap no-op if inactive.
void flvPushEnqueue(int channel, const output_frame &f);

// Upstream frames were lost: flush the channel and refuse non-key frames until
// the next keyframe, so the decoder never predicts from a missing reference.
void flvPushDiscontinuity(int channel);

// Enqueue an AAC frame for this channel (one shared mic fans out to every
// active channel). Listen/mic-out only; talkback is a separate mechanism.
void flvPushEnqueueAudio(int channel, const output_frame &f);

// Enqueue a raw Opus packet (TOC byte first) for the type-10 track (main.cpp's
// transcode); without type 10 the controller has no audio track for live view.
void flvPushEnqueueOpus(int channel, const output_frame &f);

// Told by main() whether the AAC->Opus transcode is live; only then is the
// type-10 config tag advertised, so the controller never sees an empty track.
void flvPushSetOpusEnabled(bool enabled);

// True while the controller muted the mic (MUTE on). Audio is replaced by silence,
// not stopped: the ingest errors if audio lags video by more than 1000 ms.
bool flvPushAudioMuted();

#endif
