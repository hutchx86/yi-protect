/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * yi_protect_snapshot: read the stock encoder's shared-memory frame ring, take the
 * next keyframe on a channel, decode it and write one JPEG to stdout. Our own
 * replacement for the vendored yi-hack imggrabber (Protect "GetRequest").
 */
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <utility>
#include <vector>

#include <getopt.h>
#include <strings.h>
#include <sys/stat.h>
#include <unistd.h>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/frame.h>
}
#include <jpeglib.h>

#include "FshareReader.h"
#include "bridge.h"

namespace {

const int kJpegQuality = 90;
const unsigned kAlarmSeconds = 15;  // the client also bounds the exec

struct Snap {
    int wantType = TYPE_HIGH;
    int codec = 0;                  // 0 = unknown, 1 = H.264, 2 = H.265
    std::vector<unsigned char> vps, sps, pps, jpeg;
    bool done = false;
    AVCodecContext *dec = nullptr;
    AVFrame *frame = nullptr;
};

// Open the decoder once the ring's codec is known (the encoder may be either).
bool openDecoder(Snap &s, int codec) {
    const AVCodec *c = avcodec_find_decoder(codec == 2 ? AV_CODEC_ID_HEVC : AV_CODEC_ID_H264);
    if (c == nullptr) return false;
    s.codec = codec;
    s.dec = avcodec_alloc_context3(c);
    s.frame = av_frame_alloc();
    if (s.dec == nullptr || s.frame == nullptr) return false;
    s.dec->flags |= AV_CODEC_FLAG_UNALIGNED;
    return avcodec_open2(s.dec, c, nullptr) >= 0;
}

// Next Annex-B start code at/after pos; start/scLen are its offset and length.
bool nextStart(const unsigned char *p, size_t n, size_t &pos, size_t &start, size_t &scLen) {
    for (size_t i = pos; i + 3 <= n; i++) {
        if (p[i] == 0 && p[i + 1] == 0 && p[i + 2] == 1) {
            start = i; scLen = 3; pos = i + 3; return true;
        }
        if (i + 4 <= n && p[i] == 0 && p[i + 1] == 0 && p[i + 2] == 0 && p[i + 3] == 1) {
            start = i; scLen = 4; pos = i + 4; return true;
        }
    }
    return false;
}



inline uint8_t clip8(int v) { return v < 0 ? 0 : (v > 255 ? 255 : (uint8_t)v); }

// BT.601 limited-range YUV420P -> RGB -> baseline JPEG (libjpeg memory dest).
bool frameToJpeg(Snap &s, std::vector<unsigned char> &out) {
    AVFrame *f = s.frame;
    int w = f->width, h = f->height;
    if (w <= 0 || h <= 0) return false;
    if (f->format != AV_PIX_FMT_YUV420P && f->format != AV_PIX_FMT_YUVJ420P) {
        std::fprintf(stderr, "yi_protect_snapshot: unsupported pixel format %d\n", f->format);
        return false;
    }

    struct jpeg_compress_struct cinfo;
    struct jpeg_error_mgr jerr;
    unsigned char *mem = nullptr;
    unsigned long memlen = 0;
    std::vector<unsigned char> rgb((size_t)w * 3);

    cinfo.err = jpeg_std_error(&jerr);
    jpeg_create_compress(&cinfo);
    jpeg_mem_dest(&cinfo, &mem, &memlen);
    cinfo.image_width = w;
    cinfo.image_height = h;
    cinfo.input_components = 3;
    cinfo.in_color_space = JCS_RGB;
    jpeg_set_defaults(&cinfo);
    jpeg_set_quality(&cinfo, kJpegQuality, TRUE);
    jpeg_start_compress(&cinfo, TRUE);

    while (cinfo.next_scanline < cinfo.image_height) {
        int y = (int)cinfo.next_scanline;
        const unsigned char *yp = f->data[0] + (size_t)y * f->linesize[0];
        const unsigned char *up = f->data[1] + (size_t)(y / 2) * f->linesize[1];
        const unsigned char *vp = f->data[2] + (size_t)(y / 2) * f->linesize[2];
        for (int x = 0; x < w; x++) {
            int yy = yp[x];
            int u = up[x / 2] - 128;
            int v = vp[x / 2] - 128;
            int c = yy - 16;
            if (c < 0) c = 0;
            rgb[x * 3 + 0] = clip8((298 * c + 409 * v + 128) >> 8);
            rgb[x * 3 + 1] = clip8((298 * c - 100 * u - 208 * v + 128) >> 8);
            rgb[x * 3 + 2] = clip8((298 * c + 516 * u + 128) >> 8);
        }
        JSAMPROW row[1] = { rgb.data() };
        jpeg_write_scanlines(&cinfo, row, 1);
    }

    jpeg_finish_compress(&cinfo);
    out.assign(mem, mem + memlen);
    std::free(mem);
    jpeg_destroy_compress(&cinfo);
    return !out.empty();
}

bool decodeAu(Snap &s, const std::vector<unsigned char> &au) {
    AVPacket *pkt = av_packet_alloc();
    if (pkt == nullptr) return false;
    if (av_new_packet(pkt, (int)au.size()) < 0) {
        av_packet_free(&pkt);
        return false;
    }
    std::memcpy(pkt->data, au.data(), au.size());
    int r = avcodec_send_packet(s.dec, pkt);
    av_packet_free(&pkt);
    if (r < 0) return false;
    if (avcodec_receive_frame(s.dec, s.frame) < 0) return false;

    // Honour an SPS crop even when it breaks plane alignment (e.g. 1936->1920).
    av_frame_apply_cropping(s.frame, AV_FRAME_CROP_UNALIGNED);
    s.jpeg.clear();
    if (!frameToJpeg(s, s.jpeg)) return false;
    s.done = true;
    return true;
}

// Cache the parameter sets and, on the first IDR, decode SPS+PPS+IDR and stop.
bool onFrame(void *v, int frameType, std::vector<unsigned char> &&payload,
             uint32_t time, uint16_t streamCounter) {
    (void)time;
    (void)streamCounter;
    Snap *s = (Snap *)v;
    if (frameType != s->wantType) return true;

    bool hasIdr = false;
    size_t pos = 0, start, scLen;
    while (nextStart(payload.data(), payload.size(), pos, start, scLen)) {
        const unsigned char *p = payload.data() + start + scLen;
        int h264t = p[0] & 0x1f;             // H.264 nal_unit_type
        int hevcT = (p[0] >> 1) & 0x3f;      // HEVC nal_unit_type

        // Parameter sets are the only NAL in their ring entry, so a match ends
        // the scan. The codec is detected from the first recognised NAL.
        if (s->codec == 0) {
            if (h264t == 7 || h264t == 8 || h264t == 5) s->codec = 1;
            else if (hevcT == 32 || hevcT == 33 || hevcT == 34 ||
                     (hevcT >= 16 && hevcT <= 23)) s->codec = 2;
            else continue;
        }
        if (s->codec == 2) {
            if (hevcT == 32) { s->vps.assign(payload.begin() + start, payload.end()); break; }
            if (hevcT == 33) { s->sps.assign(payload.begin() + start, payload.end()); break; }
            if (hevcT == 34) { s->pps.assign(payload.begin() + start, payload.end()); break; }
            if (hevcT >= 16 && hevcT <= 23) hasIdr = true;   // BLA..CRA/IDR (IRAP)
        } else {
            if (h264t == 7) { s->sps.assign(payload.begin() + start, payload.end()); break; }
            if (h264t == 8) { s->pps.assign(payload.begin() + start, payload.end()); break; }
            if (h264t == 5) hasIdr = true;
        }
    }
    if (!hasIdr) return true;
    if (s->codec == 2) {
        if (s->vps.empty() || s->sps.empty() || s->pps.empty()) return true;
    } else if (s->sps.empty() || s->pps.empty()) {
        return true;
    }
    if (s->dec == nullptr && !openDecoder(*s, s->codec)) return true;

    std::vector<unsigned char> au;
    if (s->codec == 2) au.insert(au.end(), s->vps.begin(), s->vps.end());
    au.insert(au.end(), s->sps.begin(), s->sps.end());
    au.insert(au.end(), s->pps.begin(), s->pps.end());
    au.insert(au.end(), payload.begin(), payload.end());
    decodeAu(*s, au);
    return !s->done;
}

std::string readModelSuffix() {
    const char *prefix = std::getenv("YIP_PREFIX");
    std::string base = (prefix != nullptr && *prefix != '\0') ? prefix : "/tmp/sd/yi-protect";
    std::string path = base + "/etc/model_suffix";
    FILE *fp = std::fopen(path.c_str(), "r");
    if (fp == nullptr) return "";
    char line[64] = {0};
    char *got = std::fgets(line, sizeof(line), fp);
    std::fclose(fp);
    if (got == nullptr) return "";
    line[std::strcspn(line, "\r\n")] = '\0';
    return line;
}

void usage(const char *prog) {
    std::fprintf(stderr,
                 "Usage: %s [-m MODEL] [-r high|low] [-d]\n"
                 "  -m, --model MODEL   override the model (else model_suffix)\n"
                 "  -r, --res RES       \"high\" (default) or \"low\"\n"
                 "  -d, --debug         verbose\n",
                 prog);
}

}  // namespace

int main(int argc, char **argv) {
    std::string model;
    bool wantLow = false;
    bool debug = false;

    static struct option longOpts[] = {
        {"model", required_argument, 0, 'm'},
        {"res",   required_argument, 0, 'r'},
        {"debug", no_argument,       0, 'd'},
        {"help",  no_argument,       0, 'h'},
        {nullptr, 0, nullptr, 0},
    };
    int c;
    while ((c = getopt_long(argc, argv, "m:r:dh", longOpts, nullptr)) != -1) {
        switch (c) {
            case 'm': model = optarg; break;
            case 'r': wantLow = (strcasecmp(optarg, "low") == 0); break;
            case 'd': debug = true; break;
            default: usage(argv[0]); return 2;
        }
    }

    if (model.empty()) model = readModelSuffix();
    ModelParams mp = modelParams(model.empty() ? nullptr : model.c_str());

    struct stat st;
    if (stat(FSHARE_BUF_FILE, &st) != 0 || st.st_size <= 0) {
        std::fprintf(stderr, "yi_protect_snapshot: %s unavailable\n", FSHARE_BUF_FILE);
        return 1;
    }

    Snap s;
    s.wantType = wantLow ? TYPE_LOW : TYPE_HIGH;
    // The decoder is opened on the first keyframe, once the ring's codec
    // (H.264 or H.265) is known - the encoder is switchable.

    FshareReader::Config cfg;
    cfg.size = (size_t)st.st_size;
    cfg.offset = mp.offset;
    cfg.headerSize = mp.headerSize;
    if (debug) {
        std::fprintf(stderr, "yi_protect_snapshot: model=%s res=%s ring=%zu offset=%u header=%d\n",
                     model.empty() ? "(default)" : model.c_str(),
                     wantLow ? "low" : "high", cfg.size, cfg.offset, cfg.headerSize);
    }

    alarm(kAlarmSeconds);  // default disposition kills us if no keyframe arrives
    FshareReader reader(debug ? 1 : 0);
    reader.run(cfg, onFrame, &s);

    if (!s.done || s.jpeg.empty()) {
        std::fprintf(stderr, "yi_protect_snapshot: no keyframe\n");
        return 1;
    }
    if (std::fwrite(s.jpeg.data(), 1, s.jpeg.size(), stdout) != s.jpeg.size()) {
        std::fprintf(stderr, "yi_protect_snapshot: short write\n");
        return 1;
    }
    return 0;
}
