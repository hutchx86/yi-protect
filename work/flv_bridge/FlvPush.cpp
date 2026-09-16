// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

/*
 * FlvPush implementation. See FlvPush.h.
 */
#include "FlvPush.h"
#include "bridge.h"

#include <cstdio>
#include <cstring>
#include <strings.h>
#include <cstdint>
#include <cstdlib>
#include <vector>
#include <string>
#include <pthread.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <netdb.h>
#include <errno.h>
#include <csignal>
#include <sys/time.h>

// Set by main.cpp from -m; picks the per-model HIGH-channel resolution.
extern int model;

static double nowSeconds() {
    struct timeval tv;
    gettimeofday(&tv, nullptr);
    return (double)tv.tv_sec + (double)tv.tv_usec / 1e6;
}

static double epochMillis() {
    struct timeval tv;
    gettimeofday(&tv, nullptr);
    return (double)tv.tv_sec * 1000.0 + (double)tv.tv_usec / 1000.0;
}

namespace {

// Per-channel onMetaData fields. videoWidth/Height/Fps must match the real
// encoder output (these are real frames, not transcoded); channelId/streamId/
// videoBandwidth match the Go client's handleVideoSettings() declarations so
// both sides stay consistent.
struct ChannelState {
    output_queue flvQueue;
    // Per-channel AAC queue (one shared mic feeds every active channel).
    // cachedAsc mirrors cachedSps/Pps: derived once from the first ADTS header
    // (see parseAdtsHeader), then reused across reconnects since the encoder's
    // rate/channel count never change stream-to-stream on this hardware.
    output_queue audioQueue;
    // Opus queue, muxed as FLV tag type 10. Real cameras send both 16kHz AAC
    // (type 8, recording/mobile) and 48kHz Opus (type 10, the only audio the
    // controller's web-live path uses); without it ms sees no audio track and
    // web live is silent.
    output_queue opusQueue;
    unsigned char cachedAsc[2];
    bool haveCachedAsc;
    pthread_mutex_t stateMutex;
    volatile int activeFd;
    volatile unsigned generation;
    std::string streamName;

    // Dial target + desired state, so a push thread whose peer vanished can
    // redial the same destination. The Go client suppresses repeat CONNECTs
    // for an unchanged destination, so a peer-gone close would otherwise
    // leave the channel dead until a new streamName arrives.
    std::string host;
    int port;
    bool wantConnected;

    // Last-known SPS/PPS, cached across reconnects (guarded by stateMutex).
    // The ILFL acceptor drops the connection unless the AVC sequence header
    // ships with the FLV header/onMetaData, and a fresh SPS/PPS can take over
    // a second to appear, so reuse the cached one. SPS/PPS differ per channel
    // but never change stream-to-stream on this hardware.
    std::vector<unsigned char> cachedSps, cachedPps;
    bool haveCachedSpsPps;

    double channelId, streamId, videoBandwidth, videoFps, videoWidth, videoHeight;

    // Real measured fps from actual frame arrival timing, cached across
    // reconnects; 0 = not yet measured (first connection), then videoFps is
    // used.
    double cachedMeasuredFps;
};

ChannelState g_ch[FLV_CH_COUNT];
bool g_initDone = false;

bool nalIsSps(unsigned char hdr) { return (hdr & 0x1f) == 7; }
bool nalIsPps(unsigned char hdr) { return (hdr & 0x1f) == 8; }
bool nalIsIdr(unsigned char hdr) { return (hdr & 0x1f) == 5; }

// -------- byte buffer / socket write helpers --------

// Dumps raw bytes to a fixed per-channel file (plain open/write/close, like
// writeAll()) to rule out formatting bugs as a crash source.
const char *dumpPathFor(int channel, bool sync) {
    if (sync) {
        if (channel == FLV_CH_HIGH) return "/tmp/flvpush_sync_high.bin";
        if (channel == FLV_CH_MED) return "/tmp/flvpush_sync_medium.bin";
        return "/tmp/flvpush_sync_low.bin";
    }
    if (channel == FLV_CH_HIGH) return "/tmp/flvpush_dump_high.bin";
    if (channel == FLV_CH_MED) return "/tmp/flvpush_dump_medium.bin";
    return "/tmp/flvpush_dump_low.bin";
}

void hexDump(int channel, const char *label, const unsigned char *data, size_t len, const char *pathOverride = nullptr) {
    static const bool enabled = getenv("FLVPUSH_DUMP_RAW") != nullptr;
    if (!enabled) return;
    const char *path = pathOverride ? pathOverride : dumpPathFor(channel, false);
    int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    if (fd < 0) return;
    write(fd, data, len);
    close(fd);
    fprintf(stderr, "FlvPush[%d]: %s: wrote %zu raw bytes to %s\n", channel, label, len, path);
}

// Drains inbound bytes evostreamms sent on this write-only socket and reports
// whether the peer is still there. Without a recv(), an abandoned connection
// stays ESTABLISHED with a growing Recv-Q and we keep pushing full-res video
// into a socket nobody reads, burning CPU on an already-starved single core.
// Non-blocking recv: 0 = orderly close, any error but EAGAIN/EWOULDBLOCK =
// dead; both tear the thread down instead of writing into a void.
static bool peerStillAlive(int fd) {
    unsigned char buf[512];
    for (;;) {
        ssize_t n = recv(fd, buf, sizeof(buf), MSG_DONTWAIT);
        if (n > 0) continue; // drain and keep checking, there may be more
        if (n == 0) return false; // orderly close
        if (errno == EAGAIN || errno == EWOULDBLOCK) return true; // nothing pending, still alive
        if (errno == EINTR) continue;
        return false; // any other error: treat as dead
    }
}

bool writeAll(int fd, const unsigned char *data, size_t len) {
    size_t off = 0;
    while (off < len) {
        ssize_t n = write(fd, data + off, len - off);
        if (n <= 0) {
            if (n < 0 && errno == EINTR) continue;
            return false;
        }
        off += (size_t)n;
    }
    return true;
}

void put_u8(std::vector<unsigned char> &b, uint8_t v) { b.push_back(v); }
void put_u16(std::vector<unsigned char> &b, uint16_t v) {
    b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
void put_u24(std::vector<unsigned char> &b, uint32_t v) {
    b.push_back((v >> 16) & 0xff); b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
void put_u32(std::vector<unsigned char> &b, uint32_t v) {
    b.push_back((v >> 24) & 0xff); b.push_back((v >> 16) & 0xff);
    b.push_back((v >> 8) & 0xff); b.push_back(v & 0xff);
}
void put_bytes(std::vector<unsigned char> &b, const unsigned char *p, size_t n) {
    b.insert(b.end(), p, p + n);
}

// AMF0 string (0x02 + u16 length + bytes), for the top-level tag name.
void amf0_string_typed(std::vector<unsigned char> &b, const std::string &s) {
    put_u8(b, 0x02);
    put_u16(b, (uint16_t)s.size());
    put_bytes(b, (const unsigned char *)s.data(), s.size());
}
// AMF0 object property NAME: bare u16-length string, no type prefix (property
// VALUES are still fully typed).
void amf0_prop_name(std::vector<unsigned char> &b, const std::string &s) {
    put_u16(b, (uint16_t)s.size());
    put_bytes(b, (const unsigned char *)s.data(), s.size());
}
void amf0_prop_string(std::vector<unsigned char> &b, const std::string &name, const std::string &val) {
    amf0_prop_name(b, name);
    amf0_string_typed(b, val);
}
void amf0_prop_number(std::vector<unsigned char> &b, const std::string &name, double val) {
    amf0_prop_name(b, name);
    put_u8(b, 0x00); // AMF0 number
    unsigned char buf[8];
    memcpy(buf, &val, 8);
    // Big-endian IEEE754 on wire; host is little-endian ARM.
    for (int i = 0; i < 8; i++) b.push_back(buf[7 - i]);
}
void amf0_prop_bool(std::vector<unsigned char> &b, const std::string &name, bool val) {
    amf0_prop_name(b, name);
    put_u8(b, 0x01); // AMF0 boolean
    put_u8(b, val ? 1 : 0);
}
// Property whose value is a nested AMF0 object; caller ends it with
// amf0_object_end().
void amf0_prop_object_begin(std::vector<unsigned char> &b, const std::string &name) {
    amf0_prop_name(b, name);
    put_u8(b, 0x03); // AMF0 Object marker
}
void amf0_object_end(std::vector<unsigned char> &b) {
    put_u16(b, 0); put_u8(b, 0x09); // empty name + object-end marker
}

// Writes a complete FLV tag (11-byte header + data + PreviousTagSize) to buf.
void writeFlvTag(std::vector<unsigned char> &buf, uint8_t tagType,
                  const std::vector<unsigned char> &data, uint32_t timestampMs) {
    uint32_t dataSize = (uint32_t)data.size();
    put_u8(buf, tagType);
    put_u24(buf, dataSize);
    put_u24(buf, timestampMs & 0xffffff);
    put_u8(buf, (timestampMs >> 24) & 0xff); // timestamp extended
    put_u24(buf, 0); // stream id, always 0
    put_bytes(buf, data.data(), data.size());
    put_u32(buf, 11 + dataSize);
}

// "extendedFlv" trailer: 16 bytes after EVERY FLV tag (UniFi/evostreamms
// extension, not FLV spec): 0x00, a 3-byte clock-rate marker (video
// 0x015F90=90000 vs other 0x002B11=11025), 8 padding bytes, then a 4-byte
// big-endian ELAPSED time in that marker's own ticks.
//
// The elapsed field MUST use the trailer's own ticks, not unifi-cam-proxy's
// `elapsed * 100000`: that advances the controller's derived wall clock at
// 100000/90000 = 1.111x, so FeedData's wc/now diff grows ~11% of connection
// age until every frame is rejected. Real hardware advances it at ~90000
// ticks/s, letting the ingest connection live indefinitely.
void writeTimestampTrailer(std::vector<unsigned char> &buf, bool isPacket, double elapsedSeconds) {
    put_u8(buf, 0);
    double clock;
    if (isPacket) {
        unsigned char m[11] = {1, 95, 144, 0, 0, 0, 0, 0, 0, 0, 0}; // 0x015F90 = 90000
        put_bytes(buf, m, 11);
        clock = 90000.0;
    } else {
        unsigned char m[11] = {0, 43, 17, 0, 0, 0, 0, 0, 0, 0, 0};  // 0x002B11 = 11025
        put_bytes(buf, m, 11);
        clock = 11025.0;
    }
    put_u32(buf, (uint32_t)(elapsedSeconds * clock));
}

// Same trailer with an explicit 24-bit marker + tick rate. A G3 capture showed
// the marker is the tag's own clock: video/metadata 90000, AAC its sample rate
// (16000), Opus 48000. The version above only knows 90000/11025.
void writeTimestampTrailerClock(std::vector<unsigned char> &buf, uint32_t marker,
                                double clock, double elapsedSeconds) {
    put_u8(buf, 0);
    unsigned char m[11] = {0};
    m[0] = (marker >> 16) & 0xff;
    m[1] = (marker >> 8) & 0xff;
    m[2] = marker & 0xff;
    put_bytes(buf, m, 11);
    put_u32(buf, (uint32_t)(elapsedSeconds * clock));
}

// onMetaData, field-for-field from a real UniFi camera's live tag. The old
// hand-rolled ECMA array was rejected by evostreamms's ILFL parser, so this is
// an AMF0 OBJECT (0x03, not ECMA 0x08), with no videocodecid/audiocodecid,
// audio* fields always present, and streamName being the controller-issued
// per-session token. Properties in alphabetical order.
std::vector<unsigned char> buildOnMetaData(const std::string &streamName,
                                            double channelId, double streamId,
                                            double videoBandwidth, double videoFps,
                                            double videoWidth, double videoHeight,
                                            bool hasAudio, double audioChannels,
                                            double audioFrequency) {
    std::vector<unsigned char> body;
    amf0_string_typed(body, "onMetaData");
    put_u8(body, 0x03); // AMF0 Object marker
    // Real G3 declares its AAC bitrate (64000); a hardcoded 0 logged our audio
    // as "0 b/s" and left the web-live Opus transcode with no audio track.
    amf0_prop_number(body, "audioBandwidth", 64000);
    amf0_prop_number(body, "audioChannels", audioChannels);
    amf0_prop_number(body, "audioFrequency", audioFrequency);
    amf0_prop_number(body, "channelId", channelId);
    // Real cameras declare true (UniFi extendedFlv wrapping), which we don't
    // implement; true passed onMetaData but then failed with "Unexpected
    // clockrate value", so false for our plain spec-compliant tags.
    amf0_prop_bool(body, "extendedFormat", false);
    // True once an ASC is cached for this channel (see call site); the one
    // real onMetaData capture had hasAudio=false, never confirmed on an
    // audio-enabled camera.
    amf0_prop_bool(body, "hasAudio", hasAudio);
    amf0_prop_bool(body, "hasVideo", true);
    amf0_prop_number(body, "streamId", streamId);
    amf0_prop_string(body, "streamName", streamName);
    amf0_prop_number(body, "videoBandwidth", videoBandwidth);
    amf0_prop_number(body, "videoFps", videoFps);
    amf0_prop_number(body, "videoHeight", videoHeight);
    amf0_prop_number(body, "videoWidth", videoWidth);
    put_u16(body, 0); put_u8(body, 0x09); // end marker (empty name + object-end)
    return body;
}

// "onClockSync" / "onMpma": UniFi extension (not FLV spec), REQUIRED -- without
// periodic onClockSync the controller fails every chunk with "no last known
// clock sync - dropping". Injected every 5s (see pushThreadMain's lastSyncTime),
// each followed by its own non-packet trailer.
std::vector<unsigned char> buildOnClockSync(double streamClockMs, double wallClockMs) {
    std::vector<unsigned char> body;
    amf0_string_typed(body, "onClockSync");
    put_u8(body, 0x03); // AMF0 Object marker
    amf0_prop_number(body, "streamClock", streamClockMs);
    amf0_prop_number(body, "streamClockBase", 0);
    amf0_prop_number(body, "wallClock", wallClockMs);
    amf0_object_end(body);
    return body;
}

std::vector<unsigned char> buildOnMpma() {
    std::vector<unsigned char> body;
    amf0_string_typed(body, "onMpma");
    put_u8(body, 0x03); // AMF0 Object marker
    // Encoder bitrate bounds for the controller's adaptive-bitrate logic;
    // exact values need not match our encoder, only the tag shape.
    amf0_prop_object_begin(body, "cs");
    amf0_prop_number(body, "cur", 1500000);
    amf0_prop_number(body, "max", 1500000);
    amf0_prop_number(body, "min", 1500000);
    amf0_object_end(body);
    amf0_prop_object_begin(body, "m");
    amf0_prop_number(body, "cur", 750000);
    amf0_prop_number(body, "max", 1500000);
    amf0_prop_number(body, "min", 750000);
    amf0_object_end(body);
    amf0_prop_number(body, "r", 0);
    amf0_prop_object_begin(body, "sp");
    amf0_prop_number(body, "cur", 1500000);
    amf0_prop_number(body, "max", 1500000);
    amf0_prop_number(body, "min", 150000);
    amf0_object_end(body);
    amf0_prop_number(body, "t", 750000);
    amf0_object_end(body);
    return body;
}

// AVCDecoderConfigurationRecord (AVCPacketType=0), built from real SPS/PPS.
std::vector<unsigned char> buildAvcSequenceHeader(const std::vector<unsigned char> &sps,
                                                   const std::vector<unsigned char> &pps) {
    std::vector<unsigned char> tag;
    put_u8(tag, 0x17); // frametype=1 (key), codecid=7 (AVC)
    put_u8(tag, 0x00); // AVCPacketType=0 (seq header)
    put_u24(tag, 0);   // composition time = 0

    std::vector<unsigned char> rec;
    put_u8(rec, 0x01); // configurationVersion
    put_u8(rec, sps.size() > 1 ? sps[1] : 0x64); // profile_idc
    put_u8(rec, sps.size() > 2 ? sps[2] : 0x00); // profile_compat
    put_u8(rec, sps.size() > 3 ? sps[3] : 0x1f); // level_idc
    put_u8(rec, 0xff); // 6 bits reserved(111111) + lengthSizeMinusOne=3 (4-byte lengths)
    put_u8(rec, 0xe1); // 3 bits reserved(111) + numOfSPS=1
    put_u16(rec, (uint16_t)sps.size());
    put_bytes(rec, sps.data(), sps.size());
    put_u8(rec, 0x01); // numOfPPS
    put_u16(rec, (uint16_t)pps.size());
    put_bytes(rec, pps.data(), pps.size());

    put_bytes(tag, rec.data(), rec.size());
    return tag;
}

std::vector<unsigned char> buildNaluTag(const unsigned char *nal, size_t len, bool isKey) {
    std::vector<unsigned char> tag;
    put_u8(tag, isKey ? 0x17 : 0x27); // frametype (1=key,2=inter), codecid=7
    put_u8(tag, 0x01); // AVCPacketType=1 (NALU)
    put_u24(tag, 0);   // composition time
    put_u32(tag, (uint32_t)len); // AVCC 4-byte length prefix
    put_bytes(tag, nal, len);
    return tag;
}

// -------- Audio: ADTS -> FLV/AAC tags --------
//
// Each audio queue frame is a 7-byte ADTS header (no CRC) + raw AAC payload.
// The sample-rate index/channel config are read from each real frame's own ADTS
// header, so no rate is hardcoded; object type is hardcoded AAC LC (2).
// The FLV envelope is fixed 0xAF (SoundFormat=10/AAC, SoundRate=3, SoundSize=1,
// SoundType=1; AACPacketType 0=seq header/1=raw). AAC tags use isPacket=true,
// the same trailer marker as video -- revisit if evostreamms rejects audio.

static bool adtsSyncWordOk(const unsigned char *d, size_t len) {
    return len >= 7 && d[0] == 0xFF && (d[1] & 0xF0) == 0xF0;
}

static const unsigned kAdtsSampleRateTable[16] = {
    96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
    16000, 12000, 11025, 8000, 7350, 0, 0, 0
};

// Parses sampling_frequency_index (bits 18-21) and channel_configuration
// (bits 23-25) straight out of a 7-byte ADTS header's byte 2/3, per the
// standard ADTS fixed-header bit layout.
static void parseAdtsHeader(const unsigned char *d, uint8_t *samplingFreqIdx, uint8_t *channelConfig) {
    *samplingFreqIdx = (d[2] >> 2) & 0x0F;
    *channelConfig = ((d[2] & 0x01) << 2) | ((d[3] >> 6) & 0x03);
}

static void buildAudioSpecificConfig(uint8_t samplingFreqIdx, uint8_t channelConfig, unsigned char asc[2]) {
    const uint8_t audioObjectType = 2; // AAC LC -- see comment block above
    asc[0] = (audioObjectType << 3) | (samplingFreqIdx >> 1);
    asc[1] = (samplingFreqIdx << 7) | (channelConfig << 3);
}

// One digitally-silent raw AAC-LC mono frame (no ADTS header), substituted
// while the controller muted the mic. Real hardware mutes the ADC gain, so the
// encoder keeps emitting silent audio tags rather than stopping the track; we
// have no AAC encoder, so this pre-encoded frame is used. Generic mono LC
// raw_data_block (1024 samples), valid at any declared sample rate; verified to
// decode to -91 dB. Regenerate from an ffmpeg anullsrc->aac stream and take a
// steady-state frame's 4-byte payload.
static const unsigned char kSilentAacMono[] = {0x01, 0x18, 0x20, 0x07};

std::vector<unsigned char> buildAacSequenceHeaderTag(const unsigned char asc[2]) {
    std::vector<unsigned char> tag;
    put_u8(tag, 0xAF); // SoundFormat=10(AAC), SoundRate=3, SoundSize=1, SoundType=1 (fixed, spec convention)
    put_u8(tag, 0x00); // AACPacketType=0 (sequence header)
    put_bytes(tag, asc, 2);
    return tag;
}

std::vector<unsigned char> buildAacRawTag(const unsigned char *aac, size_t len) {
    std::vector<unsigned char> tag;
    tag.reserve(len + 2);
    put_u8(tag, 0xAF);
    put_u8(tag, 0x01); // AACPacketType=1 (raw AAC frame)
    put_bytes(tag, aac, len);
    return tag;
}

// Splits an Annex-B buffer (possibly several start-code delimited NALs) into
// NAL [begin, end) spans, start codes excluded.
void splitAnnexB(const std::vector<unsigned char> &buf, std::vector<std::pair<size_t, size_t>> &spans) {
    size_t n = buf.size();
    // scStart = index of the "00 00 01" (or "00 00 00 01"), nalStart = index
    // of the first byte of NAL data (right after that start code).
    std::vector<std::pair<size_t, size_t>> markers; // (scStart, nalStart)
    size_t i = 0;
    while (i + 3 <= n) {
        if (buf[i] == 0 && buf[i + 1] == 0 && buf[i + 2] == 1) {
            markers.push_back(std::make_pair(i, i + 3));
            i += 3;
            continue;
        }
        if (i + 4 <= n && buf[i] == 0 && buf[i + 1] == 0 && buf[i + 2] == 0 && buf[i + 3] == 1) {
            markers.push_back(std::make_pair(i, i + 4));
            i += 4;
            continue;
        }
        i++;
    }
    for (size_t k = 0; k < markers.size(); k++) {
        size_t begin = markers[k].second;
        size_t end = (k + 1 < markers.size()) ? markers[k + 1].first : n;
        if (end > begin) spans.push_back(std::make_pair(begin, end));
    }
}

struct PushThreadArg {
    int channel;
    unsigned generation;
};

} // namespace

bool flvPushActive(int channel) {
    if (channel < 0 || channel >= FLV_CH_COUNT) return false;
    return g_ch[channel].activeFd >= 0;
}

// Mic mute state, set by the control FIFO's `MUTE on|off` when the
// controller's ChangeVideoSettings audio.volume hits 0 (see FlvPush.h).
static volatile int g_audioMuted = 0;

bool flvPushAudioMuted() { return g_audioMuted != 0; }

// Measures real fps from wall-clock inter-frame intervals over a rolling
// window (see ChannelState::cachedMeasuredFps). Uses arrival time, not
// f.time, whose units are uncharacterized. Called once per real encoder frame
// regardless of viewers, so it reflects genuine encoder timing.
static void measureFps(ChannelState &c, int channel) {
    static double lastFrameWallSec[FLV_CH_COUNT] = {0};
    static double intervalSum[FLV_CH_COUNT] = {0};
    static int intervalCount[FLV_CH_COUNT] = {0};
    const int kWindowFrames = 30; // recompute roughly twice a second at 15fps, once a second at 30fps

    double now = nowSeconds();
    double last = lastFrameWallSec[channel];
    lastFrameWallSec[channel] = now;
    if (last <= 0) return; // first frame ever seen on this channel: no interval yet

    double dt = now - last;
    if (dt <= 0 || dt > 1.0) return; // guard against clock weirdness or a real gap (reconnect, stall)

    intervalSum[channel] += dt;
    intervalCount[channel]++;
    if (intervalCount[channel] >= kWindowFrames) {
        double avgIntervalSec = intervalSum[channel] / intervalCount[channel];
        double measuredFps = avgIntervalSec > 0 ? (1.0 / avgIntervalSec) : 0;
        intervalSum[channel] = 0;
        intervalCount[channel] = 0;
        if (measuredFps > 1.0 && measuredFps < 120.0) { // sanity bounds, reject garbage
            pthread_mutex_lock(&c.stateMutex);
            double prev = c.cachedMeasuredFps;
            c.cachedMeasuredFps = measuredFps;
            pthread_mutex_unlock(&c.stateMutex);
            if (prev <= 0 || (measuredFps > prev * 1.2 || measuredFps < prev * 0.8)) {
                fprintf(stderr, "FlvPush[%d]: measured fps=%.2f (was declaring hardcoded/previous=%.2f)\n",
                        channel, measuredFps, prev);
            }
        }
    }
}

void flvPushEnqueue(int channel, const output_frame &f) {
    if (channel < 0 || channel >= FLV_CH_COUNT) return;
    if (!flvPushActive(channel)) return;
    ChannelState &c = g_ch[channel];
    measureFps(c, channel);
    static unsigned dbgCount[FLV_CH_COUNT] = {0};
    dbgCount[channel]++;
    if (dbgCount[channel] <= 5 || dbgCount[channel] % 50 == 0) {
        fprintf(stderr, "FlvPush[%d]: enqueue #%u, frame.size=%zu, time=%u\n",
                channel, dbgCount[channel], f.frame.size(), f.time);
    }
    pthread_mutex_lock(&c.flvQueue.mutex);
    c.flvQueue.frame_queue.push(f); // copy (queue item is small: one frame's worth)
    while (c.flvQueue.frame_queue.size() > MAX_QUEUE_SIZE) c.flvQueue.frame_queue.pop();
    pthread_mutex_unlock(&c.flvQueue.mutex);
}

// AAC fan-out, same per-active-channel pattern as flvPushEnqueue.
// Frame format: 7-byte ADTS header + raw AAC payload (see audio comment block).
void flvPushEnqueueAudio(int channel, const output_frame &f) {
    if (channel < 0 || channel >= FLV_CH_COUNT) return;
    if (!flvPushActive(channel)) return;
    ChannelState &c = g_ch[channel];
    pthread_mutex_lock(&c.audioQueue.mutex);
    c.audioQueue.frame_queue.push(f);
    while (c.audioQueue.frame_queue.size() > MAX_QUEUE_SIZE) c.audioQueue.frame_queue.pop();
    pthread_mutex_unlock(&c.audioQueue.mutex);
}

// Set by main() once it knows whether the AAC->Opus transcode is live.
static bool g_opusEnabled = false;
void flvPushSetOpusEnabled(bool enabled) { g_opusEnabled = enabled; }

// Push a raw Opus packet (TOC byte first) for the type-10 track; same fan-out
// and bounding as flvPushEnqueueAudio.
void flvPushEnqueueOpus(int channel, const output_frame &f) {
    if (channel < 0 || channel >= FLV_CH_COUNT) return;
    if (!flvPushActive(channel)) return;
    ChannelState &c = g_ch[channel];
    pthread_mutex_lock(&c.opusQueue.mutex);
    c.opusQueue.frame_queue.push(f);
    while (c.opusQueue.frame_queue.size() > MAX_QUEUE_SIZE) c.opusQueue.frame_queue.pop();
    pthread_mutex_unlock(&c.opusQueue.mutex);
}

static int connectTo(const std::string &host, int port) {
    struct addrinfo hints, *res = nullptr;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_INET;
    hints.ai_socktype = SOCK_STREAM;
    char portStr[16];
    snprintf(portStr, sizeof(portStr), "%d", port);
    if (getaddrinfo(host.c_str(), portStr, &hints, &res) != 0 || !res) return -1;
    int fd = socket(res->ai_family, res->ai_socktype, res->ai_protocol);
    if (fd < 0) { freeaddrinfo(res); return -1; }
    if (connect(fd, res->ai_addr, res->ai_addrlen) != 0) {
        close(fd);
        freeaddrinfo(res);
        return -1;
    }
    freeaddrinfo(res);
    int one = 1;
    setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
    return fd;
}

// A push thread that loses its peer re-arms a connection to the same
// destination. `generation` gates the retry: a newer CONNECT bumps it and a
// DISCONNECT clears wantConnected, so explicit controller actions always win.
static void doConnect(int channel, const std::string &host, int port, const std::string &streamName);

// Serializes connection setup/teardown: retry threads race the FIFO control
// thread, and doConnect's generation/activeFd handoff assumes one setup at a
// time.
static pthread_mutex_t g_connectMutex = PTHREAD_MUTEX_INITIALIZER;

#define FLV_RETRY_INTERVAL_MS 1000

struct RetryArg {
    int channel;
    unsigned generation;
    std::string host;
    int port;
    std::string streamName;
};

static void *retryThreadMain(void *arg) {
    RetryArg *ra = (RetryArg *)arg;
    ChannelState &c = g_ch[ra->channel];
    usleep(FLV_RETRY_INTERVAL_MS * 1000);
    pthread_mutex_lock(&c.stateMutex);
    bool still = (c.generation == ra->generation && c.wantConnected);
    pthread_mutex_unlock(&c.stateMutex);
    if (still) {
        fprintf(stderr, "FlvPush[%d]: reconnecting to %s:%d streamName=%s\n",
                ra->channel, ra->host.c_str(), ra->port, ra->streamName.c_str());
        doConnect(ra->channel, ra->host, ra->port, ra->streamName);
    }
    delete ra;
    return nullptr;
}

static void scheduleReconnect(int channel, unsigned generation,
                              const std::string &host, int port,
                              const std::string &streamName) {
    RetryArg *ra = new RetryArg{channel, generation, host, port, streamName};
    pthread_t t;
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    pthread_create(&t, &attr, retryThreadMain, (void *)ra);
    pthread_attr_destroy(&attr);
}

static void *pushThreadMain(void *arg) {
    PushThreadArg *pa = (PushThreadArg *)arg;
    int channel = pa->channel;
    unsigned myGeneration = pa->generation;
    delete pa;
    ChannelState &c = g_ch[channel];

    int fd = c.activeFd;
    std::string streamName = c.streamName;

    std::vector<unsigned char> out;
    out.reserve(65536);
    bool haveSps = false, havePps = false, sentSeqHeader = false;
    std::vector<unsigned char> sps, pps;
    // Mirrors haveSps/havePps/sentSeqHeader above: one AAC sequence-header
    // tag sent once per connection, from cached ASC bytes if a prior
    // connection on this channel already derived them.
    bool sentAacSeqHeader = false;
    unsigned char asc[2] = {0, 0};
    uint64_t bytesWritten = 0;
    bool ok = true;

    // Seeded to the connection-start time (below) so the periodic 5s
    // onClockSync/onMpma resync doesn't immediately re-fire a duplicate pair
    // after the initial connection-start write, which already sends one.
    double lastSyncTime = 0;

    // FLV tag timestamps and the extendedFlv trailer's elapsed field are
    // relative to this per-connection start, reset on every reconnect. Every
    // reconnect gets a brand-new streamName token and ingest point from the
    // controller, so evostreamms already treats it as a new stream; keeping
    // the values connection-relative keeps them small. Process-lifetime and
    // persisted-clock variants were tried and rejected by the controller
    // (growing or constant wc/now diff); do not retry them without first
    // reproducing ms's actual wall-clock formula.
    double connectionStart = nowSeconds();
    double elapsed = nowSeconds() - connectionStart;

    // Opus PTS grid: the type-10 encoder produces ~3-4 frames at a time (one
    // AAC frame's worth) but each is a continuous 20 ms, so write-time stamping
    // made several frames share a millisecond and then jump (measured: most
    // common inter-frame gap 0 ms, vs a smooth 20 ms on real hardware). ms and
    // the player schedule Opus by these timestamps, so the bursty grid made
    // web-live audio garbled/robotic. Stamp them on their own monotonic 20 ms
    // grid anchored to the connection clock, re-anchoring only if it drifts
    // far behind (a real source dropout).
    long opusPtsIndex = -1;

    // FLV file header: "FLV", version 1, flags=0x07 (matches what real
    // UniFi cameras send -- audio+video bits both set even though this
    // stream is video-only; evostreamms's ILFL acceptor was captured live
    // accepting exactly this from real hardware, see buildOnMetaData()
    // comment), header size 9, then PreviousTagSize0=0.
    put_bytes(out, (const unsigned char *)"FLV", 3);
    put_u8(out, 1);
    put_u8(out, 0x07);
    put_u32(out, 9);
    put_u32(out, 0);

    // Fetch any cached ASC before onMetaData so hasAudio/audioChannels/
    // audioFrequency reflect reality after the first connection. A genuine
    // first connection declares hasAudio=false (see buildOnMetaData).
    pthread_mutex_lock(&c.stateMutex);
    bool haveCachedAscLocal = c.haveCachedAsc;
    if (haveCachedAscLocal) { asc[0] = c.cachedAsc[0]; asc[1] = c.cachedAsc[1]; }
    pthread_mutex_unlock(&c.stateMutex);
    double metaAudioChannels = 0, metaAudioFrequency = 0;
    if (haveCachedAscLocal) {
        uint8_t sfi = ((asc[0] & 0x07) << 1) | (asc[1] >> 7);
        uint8_t chCfg = (asc[1] >> 3) & 0x0F;
        metaAudioFrequency = kAdtsSampleRateTable[sfi & 0x0F];
        metaAudioChannels = chCfg;
    }
    // Use measured fps from a prior connection if available, else videoFps.
    pthread_mutex_lock(&c.stateMutex);
    double measuredFpsLocal = c.cachedMeasuredFps;
    pthread_mutex_unlock(&c.stateMutex);
    double effectiveFps = measuredFpsLocal > 0 ? measuredFpsLocal : c.videoFps;
    std::vector<unsigned char> meta = buildOnMetaData(streamName,
        c.channelId, c.streamId, c.videoBandwidth, effectiveFps, c.videoWidth, c.videoHeight,
        haveCachedAscLocal, metaAudioChannels, metaAudioFrequency);
    writeFlvTag(out, 18, meta, (uint32_t)(elapsed * 1000.0));
    writeTimestampTrailer(out, false, elapsed);

    // onMpma then onClockSync in this SAME initial write, before any cached
    // seq headers, matching a real G3's connection-start byte order.
    double wallNowInit = epochMillis();
    std::vector<unsigned char> mpmaInit = buildOnMpma();
    writeFlvTag(out, 18, mpmaInit, (uint32_t)(elapsed * 1000.0));
    writeTimestampTrailer(out, false, elapsed);
    std::vector<unsigned char> csInit = buildOnClockSync((double)(elapsed * 1000.0), wallNowInit);
    writeFlvTag(out, 18, csInit, (uint32_t)(elapsed * 1000.0));
    writeTimestampTrailer(out, false, elapsed);

    // If SPS/PPS are cached from a prior connection, append the AVC sequence
    // header here so it ships in the same write() as the FLV header/onMetaData
    // (see ChannelState::cachedSps).
    pthread_mutex_lock(&c.stateMutex);
    bool haveCached = c.haveCachedSpsPps;
    if (haveCached) { sps = c.cachedSps; pps = c.cachedPps; }
    pthread_mutex_unlock(&c.stateMutex);
    if (haveCached) {
        std::vector<unsigned char> seq = buildAvcSequenceHeader(sps, pps);
        writeFlvTag(out, 9, seq, (uint32_t)(elapsed * 1000.0));
        writeTimestampTrailer(out, true, elapsed);
        haveSps = havePps = sentSeqHeader = true;
    }

    // Cached ASC; the AAC trailer marker is its sample rate (16000), not 90000.
    if (haveCachedAscLocal) {
        std::vector<unsigned char> aacSeq = buildAacSequenceHeaderTag(asc);
        writeFlvTag(out, 8, aacSeq, (uint32_t)(elapsed * 1000.0));
        writeTimestampTrailerClock(out, 0x003E80, 16000.0, elapsed);
        sentAacSeqHeader = true;
    }

    // Opus track (type 10): real cameras send a 4-byte config tag
    // (0xcf 00 03 02) at connection start, 48000 trailer marker. The
    // controller's web/desktop live view is Opus-only.
    if (g_opusEnabled) {
        unsigned char opusCfg[4] = {0xcf, 0x00, 0x03, 0x02};
        std::vector<unsigned char> cfg(opusCfg, opusCfg + 4);
        writeFlvTag(out, 10, cfg, (uint32_t)(elapsed * 1000.0));
        writeTimestampTrailerClock(out, 0x00BB80, 48000.0, elapsed);
    }

    hexDump(channel, "initial write (header+onMetaData+onMpma+onClockSync[+seqHeader])", out.data(), out.size());
    ok = writeAll(fd, out.data(), out.size());
    if (ok) bytesWritten += out.size();
    lastSyncTime = nowSeconds();

    while (ok && c.generation == myGeneration) {
        if (!peerStillAlive(fd)) {
            fprintf(stderr, "FlvPush[%d]: peer gone (recv detected close/error), tearing down\n", channel);
            ok = false;
            break;
        }

        // Drain pending AAC frames each iteration, interleaving audio/video
        // tags on the same write-time clock basis as video.
        for (;;) {
            output_frame af;
            bool gotAudio = false;
            pthread_mutex_lock(&c.audioQueue.mutex);
            if (!c.audioQueue.frame_queue.empty()) {
                af = std::move(c.audioQueue.frame_queue.front());
                c.audioQueue.frame_queue.pop();
                gotAudio = true;
            }
            pthread_mutex_unlock(&c.audioQueue.mutex);
            if (!gotAudio) break;
            if (!adtsSyncWordOk(af.frame.data(), af.frame.size())) continue;

            if (!sentAacSeqHeader) {
                uint8_t sfi, chCfg;
                parseAdtsHeader(af.frame.data(), &sfi, &chCfg);
                buildAudioSpecificConfig(sfi, chCfg, asc);
                pthread_mutex_lock(&c.stateMutex);
                c.cachedAsc[0] = asc[0]; c.cachedAsc[1] = asc[1];
                c.haveCachedAsc = true;
                pthread_mutex_unlock(&c.stateMutex);
                std::vector<unsigned char> aacSeq = buildAacSequenceHeaderTag(asc);
                double seqElapsed = nowSeconds() - connectionStart;
                out.clear();
                writeFlvTag(out, 8, aacSeq, (uint32_t)(seqElapsed * 1000.0));
                writeTimestampTrailerClock(out, 0x003E80, 16000.0, seqElapsed);
                ok = writeAll(fd, out.data(), out.size());
                if (!ok) break;
                bytesWritten += out.size();
                sentAacSeqHeader = true;
            }

            // Same write-time wall-clock basis as video. The old frame-
            // duration-paced clock drifted ~10s behind video when frames
            // dropped, tripping ms's 1000ms A/V threshold and stalling live view.
            double aTagElapsed = nowSeconds() - connectionStart;
            uint32_t aTagMs = (uint32_t)(aTagElapsed * 1000.0);

            const unsigned char *rawAac = af.frame.data() + 7;
            size_t rawLen = af.frame.size() - 7;
            // Muted: keep cadence, carry silence. The ring mic is mono; only
            // substitute for mono so a stereo decoder isn't fed a mono frame.
            if (g_audioMuted) {
                uint8_t sfi = 0, chCfg = 0;
                parseAdtsHeader(af.frame.data(), &sfi, &chCfg);
                if (chCfg == 1) {
                    rawAac = kSilentAacMono;
                    rawLen = sizeof(kSilentAacMono);
                }
            }
            std::vector<unsigned char> aacTag = buildAacRawTag(rawAac, rawLen);
            out.clear();
            writeFlvTag(out, 8, aacTag, aTagMs);
            writeTimestampTrailerClock(out, 0x003E80, 16000.0, aTagElapsed);
            ok = writeAll(fd, out.data(), out.size());
            if (!ok) break;
            bytesWritten += out.size();
        }
        if (!ok) break;

        // Drain the Opus track the same way; raw packet as tag data, 48000
        // trailer marker.
        for (;;) {
            output_frame ofr;
            bool gotOpus = false;
            pthread_mutex_lock(&c.opusQueue.mutex);
            if (!c.opusQueue.frame_queue.empty()) {
                ofr = std::move(c.opusQueue.frame_queue.front());
                c.opusQueue.frame_queue.pop();
                gotOpus = true;
            }
            pthread_mutex_unlock(&c.opusQueue.mutex);
            if (!gotOpus) break;
            double nowE = nowSeconds() - connectionStart;
            if (opusPtsIndex < 0) {
                opusPtsIndex = (long)(nowE * 50.0);          // 20 ms grid
            } else if ((double)opusPtsIndex / 50.0 < nowE - 0.25) {
                opusPtsIndex = (long)(nowE * 50.0);          // dropped behind: re-anchor
            }
            double oTagElapsed = (double)opusPtsIndex / 50.0;
            opusPtsIndex++;
            uint32_t oTagMs = (uint32_t)(oTagElapsed * 1000.0);
            out.clear();
            writeFlvTag(out, 10, ofr.frame, oTagMs);
            writeTimestampTrailerClock(out, 0x00BB80, 48000.0, oTagElapsed);
            ok = writeAll(fd, out.data(), out.size());
            if (!ok) break;
            bytesWritten += out.size();
        }
        if (!ok) break;

        output_frame f;
        bool got = false;
        pthread_mutex_lock(&c.flvQueue.mutex);
        if (!c.flvQueue.frame_queue.empty()) {
            f = std::move(c.flvQueue.frame_queue.front());
            c.flvQueue.frame_queue.pop();
            got = true;
        }
        pthread_mutex_unlock(&c.flvQueue.mutex);
        if (!got) {
            static int emptyPolls[FLV_CH_COUNT] = {0};
            emptyPolls[channel]++;
            if (emptyPolls[channel] % 400 == 0) { // ~every 2s
                fprintf(stderr, "FlvPush[%d]: gen %u still waiting on empty queue (%d polls)\n",
                        channel, myGeneration, emptyPolls[channel]);
            }
            usleep(5000);
            continue;
        }

        std::vector<std::pair<size_t, size_t>> spans;
        splitAnnexB(f.frame, spans);
        if (spans.empty()) continue;

        for (size_t s = 0; ok && s < spans.size(); s++) {
            const unsigned char *nal = f.frame.data() + spans[s].first;
            size_t len = spans[s].second - spans[s].first;
            if (len == 0) continue;
            unsigned char hdr = nal[0];
            if (nalIsSps(hdr)) {
                sps.assign(nal, nal + len);
                haveSps = true;
                pthread_mutex_lock(&c.stateMutex);
                c.cachedSps = sps;
                c.haveCachedSpsPps = havePps;
                pthread_mutex_unlock(&c.stateMutex);
                continue;
            }
            if (nalIsPps(hdr)) {
                pps.assign(nal, nal + len);
                havePps = true;
                pthread_mutex_lock(&c.stateMutex);
                c.cachedPps = pps;
                c.haveCachedSpsPps = haveSps;
                pthread_mutex_unlock(&c.stateMutex);
                continue;
            }

            // Plain write-time wall-clock elapsed; frame-time (f.time) made
            // the wc/now drift worse.
            double tagElapsed = nowSeconds() - connectionStart;
            uint32_t tagMs = (uint32_t)(tagElapsed * 1000.0);

            // Every 5s, re-anchor streamClock->wallClock (own write() before
            // the triggering frame) so the controller-derived clock doesn't
            // drift with connection age.
            if (nowSeconds() - lastSyncTime >= 5.0) {
                lastSyncTime = nowSeconds();
                double wallNow = epochMillis();
                std::vector<unsigned char> sync;
                std::vector<unsigned char> cs = buildOnClockSync((double)tagMs, wallNow);
                writeFlvTag(sync, 18, cs, tagMs);
                writeTimestampTrailer(sync, false, tagElapsed);
                std::vector<unsigned char> mpma = buildOnMpma();
                writeFlvTag(sync, 18, mpma, tagMs);
                writeTimestampTrailer(sync, false, tagElapsed);
                fprintf(stderr, "FlvPush[%d]: sync inject: streamClock=%u wallClock=%.0f (bytes=%zu)\n",
                        channel, tagMs, wallNow, sync.size());
                hexDump(channel, "sync tag (onClockSync+onMpma)", sync.data(), sync.size(),
                        dumpPathFor(channel, true));
                ok = writeAll(fd, sync.data(), sync.size());
                if (!ok) break;
                bytesWritten += sync.size();
            }

            if (!sentSeqHeader) {
                if (!haveSps || !havePps) continue; // wait for both before emitting anything
                std::vector<unsigned char> seq = buildAvcSequenceHeader(sps, pps);
                out.clear();
                writeFlvTag(out, 9, seq, tagMs);
                writeTimestampTrailer(out, true, tagElapsed);
                ok = writeAll(fd, out.data(), out.size());
                if (!ok) break;
                bytesWritten += out.size();
                sentSeqHeader = true;
            }

            bool key = nalIsIdr(hdr);
            std::vector<unsigned char> tagData = buildNaluTag(nal, len, key);
            out.clear();
            writeFlvTag(out, 9, tagData, tagMs);
            writeTimestampTrailer(out, true, tagElapsed);
            ok = writeAll(fd, out.data(), out.size());
            if (!ok) break;
            bytesWritten += out.size();
        }
    }

    fprintf(stderr, "FlvPush[%d]: push thread for gen %u ending, bytes=%llu sentSeqHeader=%d\n",
            channel, myGeneration, (unsigned long long)bytesWritten, sentSeqHeader ? 1 : 0);

    pthread_mutex_lock(&c.stateMutex);
    bool shouldRetry = false;
    if (c.generation == myGeneration) {
        close(c.activeFd);
        c.activeFd = -1;
        c.streamName.clear();
        // Ended on its own (peer gone / write failure), not superseded by a
        // DISCONNECT or newer CONNECT -- re-dial. The streamName was captured
        // in a local above, which clear() doesn't touch.
        shouldRetry = c.wantConnected && !c.host.empty();
    }
    int retryPort = c.port;
    std::string retryHost = c.host;
    pthread_mutex_unlock(&c.stateMutex);

    if (shouldRetry) {
        scheduleReconnect(channel, myGeneration, retryHost, retryPort, streamName);
    }
    return nullptr;
}

static void doConnect(int channel, const std::string &host, int port, const std::string &streamName) {
    pthread_mutex_lock(&g_connectMutex);
    ChannelState &c = g_ch[channel];

    // Record the dial target + desired state up front so a failed connect (or
    // a later peer-gone) can retry the exact same destination.
    pthread_mutex_lock(&c.stateMutex);
    c.host = host;
    c.port = port;
    c.streamName = streamName;
    c.wantConnected = true;
    pthread_mutex_unlock(&c.stateMutex);

    int fd = connectTo(host, port);
    if (fd < 0) {
        fprintf(stderr, "FlvPush[%d]: connect to %s:%d failed: %s (will retry)\n",
                channel, host.c_str(), port, strerror(errno));
        pthread_mutex_unlock(&g_connectMutex);
        // Re-arm a retry; it re-checks wantConnected/generation itself.
        pthread_mutex_lock(&c.stateMutex);
        unsigned gen = c.generation;
        pthread_mutex_unlock(&c.stateMutex);
        scheduleReconnect(channel, gen, host, port, streamName);
        return;
    }

    pthread_mutex_lock(&c.stateMutex);
    if (c.activeFd >= 0) {
        // Close the old fd here, before bumping generation below: the old push
        // thread's cleanup only closes activeFd when generation still matches,
        // so it would never run. shutdown() alone does not release the fd.
        shutdown(c.activeFd, SHUT_RDWR);
        close(c.activeFd);
        c.activeFd = -1; // also signal old push thread's generation check to fail on next loop
    }
    c.generation++;
    unsigned myGeneration = c.generation;
    c.activeFd = fd;
    c.streamName = streamName;
    pthread_mutex_unlock(&c.stateMutex);
    pthread_mutex_unlock(&g_connectMutex);

    PushThreadArg *pa = new PushThreadArg{channel, myGeneration};
    pthread_t t;
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    pthread_create(&t, &attr, pushThreadMain, (void *)pa);
    pthread_attr_destroy(&attr);
}

static void doDisconnect(int channel) {
    pthread_mutex_lock(&g_connectMutex);
    ChannelState &c = g_ch[channel];
    pthread_mutex_lock(&c.stateMutex);
    // Stop pending reconnect; an explicit DISCONNECT must not be undone by retry.
    c.wantConnected = false;
    if (c.activeFd >= 0) {
        // Same fd handling as doConnect(): close directly, then orphan the thread.
        shutdown(c.activeFd, SHUT_RDWR);
        close(c.activeFd);
        c.generation++; // orphan the running push thread; it will notice and exit
        c.activeFd = -1;
        c.streamName.clear();
    }
    pthread_mutex_unlock(&c.stateMutex);
    pthread_mutex_unlock(&g_connectMutex);
}

static bool parseChannel(const char *s, int *channel) {
    if (strcmp(s, "high") == 0) { *channel = FLV_CH_HIGH; return true; }
    if (strcmp(s, "low") == 0) { *channel = FLV_CH_LOW; return true; }
    if (strcmp(s, "medium") == 0) { *channel = FLV_CH_MED; return true; }
    return false;
}

static void *ctlThreadMain(void *) {
    unlink(FLV_PUSH_FIFO);
    if (mkfifo(FLV_PUSH_FIFO, 0600) != 0 && errno != EEXIST) {
        fprintf(stderr, "FlvPush: mkfifo %s failed: %s\n", FLV_PUSH_FIFO, strerror(errno));
        return nullptr;
    }
    for (;;) {
        // Blocking open-for-read; a FIFO reader sees EOF when the writer
        // closes, so loop and reopen.
        FILE *f = fopen(FLV_PUSH_FIFO, "r");
        if (!f) { usleep(200000); continue; }
        char line[512];
        while (fgets(line, sizeof(line), f)) {
            size_t l = strlen(line);
            while (l > 0 && (line[l - 1] == '\n' || line[l - 1] == '\r')) line[--l] = 0;
            if (strncmp(line, "CONNECT ", 8) == 0) {
                char hostport[256], streamName[128], chanStr[16];
                if (sscanf(line + 8, "%255s %127s %15s", hostport, streamName, chanStr) == 3) {
                    int channel;
                    if (!parseChannel(chanStr, &channel)) {
                        fprintf(stderr, "FlvPush: CONNECT with unknown channel '%s'\n", chanStr);
                        continue;
                    }
                    std::string hp(hostport);
                    size_t colon = hp.rfind(':');
                    if (colon != std::string::npos) {
                        std::string host = hp.substr(0, colon);
                        int port = atoi(hp.c_str() + colon + 1);
                        fprintf(stderr, "FlvPush[%d]: CONNECT %s:%d streamName=%s\n", channel, host.c_str(), port, streamName);
                        doConnect(channel, host, port, streamName);
                    }
                }
            } else if (strncmp(line, "DISCONNECT", 10) == 0) {
                char chanStr[16];
                if (sscanf(line + 10, "%15s", chanStr) == 1) {
                    int channel;
                    if (parseChannel(chanStr, &channel)) {
                        fprintf(stderr, "FlvPush[%d]: DISCONNECT\n", channel);
                        doDisconnect(channel);
                    }
                }
            } else if (strncmp(line, "MUTE", 4) == 0 &&
                       (line[4] == 0 || line[4] == ' ')) {
                // Mic mute, from the controller's ChangeVideoSettings volume.
                const char *arg = line + 4;
                while (*arg == ' ') arg++;
                bool on = (strncasecmp(arg, "on", 2) == 0);
                bool off = (strncasecmp(arg, "off", 3) == 0);
                if (on || off) {
                    g_audioMuted = on ? 1 : 0;
                    fprintf(stderr, "FlvPush: mic %s (controller audio volume)\n",
                            on ? "muted" : "unmuted");
                }
            }
        }
        fclose(f);
    }
    return nullptr;
}

void flvPushInit() {
    // Without this, write() to a peer-closed socket raises SIGPIPE, whose
    // default disposition kills the whole process (the watchdog would then
    // restart it, looking like a content rejection rather than a crash).
    signal(SIGPIPE, SIG_IGN);

    if (!g_initDone) {
        ChannelState &high = g_ch[FLV_CH_HIGH];
        pthread_mutex_init(&high.flvQueue.mutex, NULL);
        pthread_mutex_init(&high.stateMutex, NULL);
        high.activeFd = -1;
        high.generation = 0;
        high.haveCachedSpsPps = false;
        // Matches video1's Go-client declaration (streamId=1, 1400000, fps 15).
        // HIGH resolution differs per model: y623 (GC3003) 2304x1296, h52ga
        // (GC2053) 1920x1080, both confirmed by SPS decode; LOW is 640x360.
        high.channelId = 0; high.streamId = 1;
        high.videoBandwidth = 1400000; high.videoFps = 15;
        if (model == H52GA) {
            high.videoWidth = 1920; high.videoHeight = 1080;
        } else {
            high.videoWidth = 2304; high.videoHeight = 1296;
        }
        high.cachedMeasuredFps = 0;

        ChannelState &low = g_ch[FLV_CH_LOW];
        pthread_mutex_init(&low.flvQueue.mutex, NULL);
        pthread_mutex_init(&low.stateMutex, NULL);
        low.activeFd = -1;
        low.generation = 0;
        low.haveCachedSpsPps = false;
        // Matches video2's declaration (streamId=2); 640x360 is the real
        // low-res encoder output, not the old 1280x720 placeholder.
        low.channelId = 1; low.streamId = 2;
        low.videoBandwidth = 500000; low.videoFps = 15;
        low.videoWidth = 640; low.videoHeight = 360;
        low.cachedMeasuredFps = 0;

        ChannelState &med = g_ch[FLV_CH_MED];
        pthread_mutex_init(&med.flvQueue.mutex, NULL);
        pthread_mutex_init(&med.stateMutex, NULL);
        med.activeFd = -1;
        med.generation = 0;
        med.haveCachedSpsPps = false;
        // Matches video3's declaration (streamId=4, 300000). 640x360 -- med
        // aliases the real LOW output, not a third hardware resolution.
        med.channelId = 2; med.streamId = 4;
        med.videoBandwidth = 300000; med.videoFps = 15;
        med.videoWidth = 640; med.videoHeight = 360;
        med.cachedMeasuredFps = 0;

        for (int i = 0; i < FLV_CH_COUNT; i++) {
            pthread_mutex_init(&g_ch[i].audioQueue.mutex, NULL);
            pthread_mutex_init(&g_ch[i].opusQueue.mutex, NULL);
            g_ch[i].haveCachedAsc = false;
            g_ch[i].port = 0;
            g_ch[i].wantConnected = false;
        }

        g_initDone = true;
    }
    pthread_t t;
    pthread_create(&t, NULL, ctlThreadMain, NULL);
}
