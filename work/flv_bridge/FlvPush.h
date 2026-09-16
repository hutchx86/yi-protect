// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

/*
 * FlvPush: the native, zero-extra-process extendedFlv pusher at the heart of
 * unifi_flv_bridge.
 *
 * Rationale: ~60MB total RAM. A separate ffmpeg process per requested stream
 * OOM-thrashed the device at 2-3 concurrent viewers. FlvPush instead taps the
 * SAME raw H.264 frames FshareReader pulls out of the stock encoder's
 * shared-memory ring (no decode/demux/remux, no extra process) and muxes them
 * into FLV tags written straight to a TCP socket.
 *
 * Three channels: HIGH (video1, real high-res encoder), LOW (video2, real
 * 640x360 encoder), MED (video3). MED aliases the SAME real LOW frames (this
 * SoC has only two independently-configured encoder outputs); video3 must be
 * fed because Protect's expanded single-camera panel requests it for every
 * quality setting, matching video3's declared 640x360 schema.
 *
 * Control FIFO /tmp/unifi_flv_bridge_ctl, newline-terminated commands:
 *   CONNECT <host:port> <streamName> <high|low|medium>
 *   DISCONNECT <high|low|medium>
 * Channels are independent: a CONNECT on one does not affect the others.
 */
#ifndef _FLV_PUSH_H
#define _FLV_PUSH_H

#include "bridge.h"

#define FLV_PUSH_FIFO "/tmp/unifi_flv_bridge_ctl"

enum { FLV_CH_HIGH = 0, FLV_CH_LOW = 1, FLV_CH_MED = 2, FLV_CH_COUNT = 3 };

// Called once from main(): create the control FIFO (if missing) and start its
// reader thread.
void flvPushInit();

// True iff this channel's push destination is connected. Gates per-frame
// duplication so an idle channel costs nothing.
bool flvPushActive(int channel);

// Enqueue one parsed video frame for its channel; cheap no-op if inactive.
void flvPushEnqueue(int channel, const output_frame &f);

// Enqueue an AAC frame for this channel (one shared mic fans out to every
// active channel). Listen/mic-out only; talkback is a separate mechanism.
void flvPushEnqueueAudio(int channel, const output_frame &f);

// Enqueue a raw Opus packet (TOC byte first) for the type-10 track, produced
// by the bridge's AAC->PCM->Opus transcode (see main.cpp). Real cameras send
// both type 8 (AAC, recording/mobile) and type 10 (Opus, web/desktop live);
// without type 10 the controller creates no audio track for live view.
void flvPushEnqueueOpus(int channel, const output_frame &f);

// Told by main() whether the AAC->Opus transcode is live; only then is the
// type-10 config tag advertised, so the controller never sees an empty track.
void flvPushSetOpusEnabled(bool enabled);

// True while the controller muted the mic (MUTE on via the control FIFO).
// FlvPush substitutes a pre-encoded silent AAC frame and main.cpp renders the
// Opus track silent, keeping the audio tag cadence so evostreamms's 1000 ms
// video-vs-last-audio threshold isn't tripped.
bool flvPushAudioMuted();

#endif
