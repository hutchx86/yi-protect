/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * unifi_flv_bridge: reads the stock video encoder's shared-memory frame ring
 * (/dev/shm/fshare_frame_buf) and pushes each video/audio stream as UniFi's
 * extendedFlv over TCP, on demand, to the Protect controller's ingest.
 *
 * FshareReader + FlvPush only; keeps the old binary's CLI for the init script.
 */
#include <cstdio>
#include <cstdlib>
#include <cstring>

#include <getopt.h>
#include <strings.h>
#include <sys/resource.h>
#include <sys/stat.h>

#include "FshareReader.h"
#include "FlvPush.h"
#include "bridge.h"

#include "neaacdec.h"
#include "opus.h"

// Cap for one decoded AAC frame's PCM (AAC-LC 1024 samples/channel, stereo).
#define AAC_MAX_NSAMPS 1024
#define AAC_MAX_NCHANS 2

// Set from -m / RRTSP_MODEL; read by FlvPush.cpp to pick the HIGH-channel
// encoder resolution for this model.
int model = Y21GA;

namespace {

int resolution = RESOLUTION_HIGH;
int audio = 1;     // 0=off, 1=PCM fifo (unsupported), 2=AAC from the ring
int debug = 0;

// ---- Opus track transcode ------------------------------------------------
// Real cameras publish two audio tracks: native AAC (type 8, recording/mobile)
// and Opus (type 10), the only audio the controller's web/desktop live view
// decodes. The ring only has AAC, so decode with FAAD2, re-buffer into 20ms
// frames, encode with libopus, and enqueue on the type-10 track; without it
// web live is silent even though the AAC track is fine.
//
// Params match a G3 capture (type-10 ~160 B = ~64 kbps, 48 kHz 20 ms). The
// ring's AAC is 16 kHz: decode, linearly upsample to 48 kHz, encode with
// OPUS_APPLICATION_AUDIO @ 64 kbps.
const int kOpusRate = 48000;
NeAACDecHandle g_aacDec = nullptr;
bool g_aacInit = false;                // NeAACDecInit has parsed the first ADTS header
OpusEncoder *g_opusEnc = nullptr;
int g_aacRate = 16000;                 // actual rate of the ring's AAC
int g_opusFrameSamples = 960;          // 20 ms @ 48 kHz
short g_pcmIn[4096];                   // mono accumulator at g_aacRate
int g_pcmInLen = 0;
double g_rsPos = 0.0;                  // resampler position, in input samples
short g_pcm48[1024];                   // resampled accumulator at 48 kHz
int g_pcm48Len = 0;

void emitOpusPacket(const unsigned char *data, size_t len) {
    output_frame of;
    of.frame.assign(data, data + len);
    of.time = 0;
    if (flvPushActive(FLV_CH_HIGH)) flvPushEnqueueOpus(FLV_CH_HIGH, of);
    if (flvPushActive(FLV_CH_LOW)) flvPushEnqueueOpus(FLV_CH_LOW, of);
    if (flvPushActive(FLV_CH_MED)) flvPushEnqueueOpus(FLV_CH_MED, of);
}

// Upsample the current g_pcmIn (at g_aacRate) to 48 kHz by linear
// interpolation and emit 20 ms Opus frames. Linear interpolation is enough
// here: the mic band is <= 8 kHz, so any imaging lands above it, and the
// Opus encoder only ever sees 48 kHz regardless of the source rate.
void flushOpusFrames() {
    const double step = (double)g_aacRate / (double)kOpusRate;
    unsigned char opusBuf[1500];
    while ((int)g_rsPos + 1 < g_pcmInLen) {
        int idx = (int)g_rsPos;
        double f = g_rsPos - idx;
        double v = g_pcmIn[idx] * (1.0 - f) + g_pcmIn[idx + 1] * f;
        g_pcm48[g_pcm48Len++] = (short)(v >= 0.0 ? v + 0.5 : v - 0.5);
        if (g_pcm48Len >= g_opusFrameSamples) {
            int n = opus_encode(g_opusEnc, g_pcm48, g_opusFrameSamples, opusBuf, sizeof(opusBuf));
            if (n > 0) emitOpusPacket(opusBuf, (size_t)n);
            g_pcm48Len = 0;
        }
        g_rsPos += step;
    }
    // Drop input samples we've fully consumed, always keeping the one that
    // straddles the current fractional position (index 0 after the shift).
    int consumed = (int)g_rsPos;
    if (consumed > g_pcmInLen - 1) consumed = g_pcmInLen - 1;
    if (consumed < 0) consumed = 0;
    if (consumed > 0) {
        memmove(g_pcmIn, g_pcmIn + consumed,
                (size_t)(g_pcmInLen - consumed) * sizeof(short));
        g_pcmInLen -= consumed;
        g_rsPos -= consumed;
    }
}

void transcodeAacToOpus(std::vector<unsigned char> &adts) {
    if (!g_aacDec) return;
    // The ring's audio is ADTS-framed. FAAD2 needs one NeAACDecInit() to read
    // the first header; it consumes that header, so the first frame's payload
    // is decoded from the remainder. Later frames are passed whole -- FAAD2
    // skips each ADTS header itself.
    const unsigned char *in = adts.data();
    size_t inLen = adts.size();
    if (!g_aacInit) {
        unsigned long sr = 0;
        unsigned char ch = 0;
        int used = NeAACDecInit(g_aacDec, const_cast<unsigned char *>(in), inLen, &sr, &ch);
        if (used < 0) return;  // header not complete yet; retry next frame
        g_aacInit = true;
        in += used;
        inLen -= (size_t)used;
    }
    if (inLen == 0) return;
    NeAACDecFrameInfo info;
    short *pcm = (short *)NeAACDecDecode(g_aacDec, &info,
                                         const_cast<unsigned char *>(in), inLen);
    if (pcm == nullptr || info.error > 0) return;

    // Create the encoder lazily, once, from the stream's actual rate.
    if (!g_opusEnc) {
        int rate = (int)info.samplerate;
        if (rate != 8000 && rate != 12000 && rate != 16000 && rate != 24000 &&
            rate != 48000) {
            rate = 16000;
        }
        g_aacRate = rate;
        int err = OPUS_OK;
        g_opusEnc = opus_encoder_create(kOpusRate, 1, OPUS_APPLICATION_AUDIO, &err);
        if (err != OPUS_OK || g_opusEnc == nullptr) {
            std::fprintf(stderr, "unifi_flv_bridge: opus_encoder_create(%d) failed: %s\n",
                         kOpusRate, opus_strerror(err));
            g_opusEnc = nullptr;
            return;
        }
        opus_encoder_ctl(g_opusEnc, OPUS_SET_BITRATE(64000));
    }

    int nch = info.channels > 0 ? info.channels : 1;
    int nsamp = (int)(info.samples / (unsigned int)nch);
    const int cap = (int)(sizeof(g_pcmIn) / sizeof(g_pcmIn[0]));
    if (nsamp > AAC_MAX_NSAMPS * AAC_MAX_NCHANS) nsamp = AAC_MAX_NSAMPS * AAC_MAX_NCHANS;
    // Muted: still decode so the encoder learns the real rate, but hand it
    // silence; FlvPush substitutes silent AAC on the type-8 track too.
    if (flvPushAudioMuted()) {
        memset(pcm, 0, sizeof(short) * (size_t)nsamp * (size_t)nch);
    }
    // info.samples counts all channels; a stereo block is interleaved, so take
    // the left channel (the ring's mic is mono in practice anyway).
    if (nch == 1) {
        for (int i = 0; i < nsamp && g_pcmInLen < cap; i++) g_pcmIn[g_pcmInLen++] = pcm[i];
    } else {
        for (int i = 0; i < nsamp && g_pcmInLen < cap; i++) g_pcmIn[g_pcmInLen++] = pcm[i * nch];
    }
    flushOpusFrames();
}

struct EmitCtx {
    int resolution;
    int audio;
};

bool emitFrame(void *vctx, int frameType, std::vector<unsigned char> &&payload,
               uint32_t time, uint16_t streamCounter) {
    (void)streamCounter;
    EmitCtx *ctx = (EmitCtx *)vctx;

    output_frame of;
    of.frame = std::move(payload);
    of.time = time;

    if (frameType == TYPE_HIGH) {
        if (ctx->resolution == RESOLUTION_HIGH || ctx->resolution == RESOLUTION_BOTH) {
            if (flvPushActive(FLV_CH_HIGH)) flvPushEnqueue(FLV_CH_HIGH, of);
        }
    } else if (frameType == TYPE_LOW) {
        // MED (video3) aliases the same real LOW frames; both destinations are
        // driven together, matching the pre-rewrite capture() behaviour.
        if (ctx->resolution != RESOLUTION_HIGH) {
            if (flvPushActive(FLV_CH_LOW)) flvPushEnqueue(FLV_CH_LOW, of);
            if (flvPushActive(FLV_CH_MED)) flvPushEnqueue(FLV_CH_MED, of);
        }
    } else if (frameType == TYPE_AAC) {
        if (ctx->audio == 2) {
            if (flvPushActive(FLV_CH_HIGH)) flvPushEnqueueAudio(FLV_CH_HIGH, of);
            if (flvPushActive(FLV_CH_LOW)) flvPushEnqueueAudio(FLV_CH_LOW, of);
            if (flvPushActive(FLV_CH_MED)) flvPushEnqueueAudio(FLV_CH_MED, of);
            transcodeAacToOpus(of.frame);
        }
    }
    return true;
}

void printUsage(const char *prog) {
    std::fprintf(stderr,
                 "\nUsage: %s [options]\n\n"
                 "\t-m MODEL   camera model (y623, h52ga, r35gb, ...)\n"
                 "\t-r RES     resolution: low, high, both or none (default high)\n"
                 "\t-a AUDIO   audio: no, aac (aac = read AAC from the ring)\n"
                 "\t-s         accepted for compatibility; no effect\n"
                 "\t-p PORT    accepted for compatibility; no effect\n"
                 "\t-d DEBUG   debug bitmask (0 none)\n"
                 "\t-h         print this help\n",
                 prog);
}

}  // namespace

int main(int argc, char **argv) {
    // Defaults mirrored from the old daemon's CLI.
    while (true) {
        static struct option longOptions[] = {
            {"model", required_argument, nullptr, 'm'},
            {"resolution", required_argument, nullptr, 'r'},
            {"audio", required_argument, nullptr, 'a'},
            {"audio_back_channel", required_argument, nullptr, 'b'},
            {"port", required_argument, nullptr, 'p'},
            {"sti", no_argument, nullptr, 's'},
            {"user", required_argument, nullptr, 'u'},
            {"password", required_argument, nullptr, 'w'},
            {"debug", required_argument, nullptr, 'd'},
            {"help", no_argument, nullptr, 'h'},
            {nullptr, 0, nullptr, 0},
        };
        int c = getopt_long(argc, argv, "m:r:a:b:p:su:w:d:h", longOptions, nullptr);
        if (c == -1) break;

        switch (c) {
        case 'm': model = parseModel(optarg); break;
        case 'r':
            if (strcasecmp("low", optarg) == 0) resolution = RESOLUTION_LOW;
            else if (strcasecmp("high", optarg) == 0) resolution = RESOLUTION_HIGH;
            else if (strcasecmp("both", optarg) == 0) resolution = RESOLUTION_BOTH;
            else if (strcasecmp("none", optarg) == 0) resolution = RESOLUTION_NONE;
            break;
        case 'a':
            if (strcasecmp("no", optarg) == 0) audio = 0;
            else if (strcasecmp("aac", optarg) == 0) audio = 2;
            else if (strcasecmp("yes", optarg) == 0 ||
                     strcasecmp("alaw", optarg) == 0 ||
                     strcasecmp("ulaw", optarg) == 0 ||
                     strcasecmp("pcm", optarg) == 0) audio = 1;
            break;
        case 'b':  // back channel (talkback) is handled by talkback_rx, not here
            break;
        case 'p':  // port; RTSP is gone, accepted for CLI compatibility
        case 's':  // SPS timing info; accepted for CLI compatibility
            break;
        case 'u':  // RTSP credentials: no longer applicable
        case 'w':
            break;
        case 'd': debug = (int)strtol(optarg, nullptr, 10); break;
        case 'h':
            printUsage(argv[0]);
            return 0;
        default:
            printUsage(argv[0]);
            return 1;
        }
    }

    // Environment overrides, kept for parity with the old daemon.
    const char *env = getenv("RRTSP_MODEL");
    if (env != nullptr) model = parseModel(env);
    env = getenv("RRTSP_RES");
    if (env != nullptr) {
        if (strcasecmp("low", env) == 0) resolution = RESOLUTION_LOW;
        else if (strcasecmp("high", env) == 0) resolution = RESOLUTION_HIGH;
        else if (strcasecmp("both", env) == 0) resolution = RESOLUTION_BOTH;
        else if (strcasecmp("none", env) == 0) resolution = RESOLUTION_NONE;
    }
    env = getenv("RRTSP_AUDIO");
    if (env != nullptr) {
        if (strcasecmp("no", env) == 0) audio = 0;
        else if (strcasecmp("aac", env) == 0) audio = 2;
        else if (strcasecmp("yes", env) == 0) audio = 1;
    }
    env = getenv("RRTSP_DEBUG");
    if (env != nullptr) debug = (int)strtol(env, nullptr, 10);

    if (audio == 1) {
        // Old PCM/WAV path read /tmp/audio_fifo through LIVE555 filters, which
        // this rewrite dropped; UniFi deployments use AAC.
        std::fprintf(stderr, "unifi_flv_bridge: PCM audio is no longer supported; "
                             "disabling audio (use -a aac)\n");
        audio = 0;
    }

    struct stat st;
    if (stat(FSHARE_BUF_FILE, &st) != 0 || st.st_size <= 0) {
        std::fprintf(stderr, "unifi_flv_bridge: cannot size %s (is rmm running?)\n",
                     FSHARE_BUF_FILE);
        return 1;
    }

    ModelParams mp = modelParams(model);
    // Match the old daemon: run slightly above the stock encoder's own
    // priority so a busy box still drains the ring in time.
    setpriority(PRIO_PROCESS, 0, -10);
    std::fprintf(stderr, "unifi_flv_bridge: model=%d resolution=%d audio=%d "
                         "ring=%lld offset=%u header=%d\n",
                 model, resolution, audio, (long long)st.st_size,
                 mp.offset, mp.headerSize);

    // Starts the control-FIFO thread and per-channel state.
    flvPushInit();

    // AAC decoder for the AAC->Opus transcode (Opus encoder is created
    // lazily on the first decoded frame, once the real sample rate is known).
    if (audio == 2) {
        g_aacDec = NeAACDecOpen();
        if (g_aacDec) {
            NeAACDecConfigurationPtr cfg = NeAACDecGetCurrentConfiguration(g_aacDec);
            cfg->defObjectType = LC;
            cfg->defSampleRate = 16000;      // ring's AAC rate (ADTS confirms it)
            cfg->outputFormat = FAAD_FMT_16BIT;
            cfg->dontUpSampleImplicitSBR = 1;  // keep the stream's real rate
            NeAACDecSetConfiguration(g_aacDec, cfg);
        } else {
            std::fprintf(stderr, "unifi_flv_bridge: NeAACDecOpen failed; Opus track disabled\n");
        }
    }
    flvPushSetOpusEnabled(audio == 2 && g_aacDec != nullptr);

    EmitCtx ctx{resolution, audio};
    FshareReader reader(debug);
    FshareReader::Config cfg;
    cfg.size = (size_t)st.st_size;
    cfg.offset = mp.offset;
    cfg.headerSize = mp.headerSize;
    reader.run(cfg, emitFrame, &ctx);

    return 0;
}
