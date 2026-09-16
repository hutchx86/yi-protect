/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 */

/*
 * talkback_rx - bridges UniFi Protect's talkback UDP stream to the speaker.
 *
 * Binds UDP :7004 and accepts two formats on the same port, told apart by the
 * first byte (ADTS sync 0xFF vs RTP version-2 0b10 -- ranges never collide):
 *   - mobile app: one ADTS AAC-LC frame per datagram, decoded with FAAD2.
 *   - desktop/web app: RTP-encapsulated Opus (12-byte RTP header plus optional
 *     extension block, Opus payload), decoded with libopus.
 *
 * Both decode to 16-bit PCM at the device's native 16 kHz mono (libopus
 * resamples internally) then share the gain-scaling + fifo-write path.
 *
 * Speaker side: PCM is written to /tmp/audio_in_fifo with inline gain scaling.
 * The amp-enable GPIO (/dev/cpld_periph) is toggled on activity: on at the first
 * packet after idle, off after IDLE_TIMEOUT_MS without packets.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <netinet/in.h>
#include <arpa/inet.h>

#include "neaacdec.h"
#include "opus.h"

// Cap for one decoded AAC frame's PCM (AAC-LC 1024 samples/channel, stereo).
#define AAC_MAX_NSAMPS 1024
#define AAC_MAX_NCHANS 2

// The speaker FIFO is fixed at 16 kHz mono; resample the decoder's rate to it,
// preserving pitch/duration (FAAD2 may emit the SBR rate).
static int resample_to_16k(const short *in, int n, unsigned long sr,
                           short *out, int cap) {
    if (n <= 0) return 0;
    if (sr == 16000 || sr == 0) {
        int c = n < cap ? n : cap;
        memcpy(out, in, (size_t)c * sizeof(short));
        return c;
    }
    double step = (double)sr / 16000.0;
    int outn = 0;
    double pos = 0.0;
    while (pos < (double)(n - 1) && outn < cap) {
        int i = (int)pos;
        double f = pos - (double)i;
        out[outn++] = (short)((double)in[i] * (1.0 - f) + (double)in[i + 1] * f);
        pos += step;
    }
    return outn;
}

#define UDP_PORT 7004
#define FIFO_PATH "/tmp/audio_in_fifo"
#define CPLD_DEV "/dev/cpld_periph"
#define CPLD_DEVICE_NUM 0x70
#define SPEAKER_ON_NUM 16
#define SPEAKER_OFF_NUM 17
#define IDLE_TIMEOUT_MS 1200
#define MAX_PKT 2048
#define OPUS_SAMPLE_RATE 16000
#define OPUS_MAX_FRAME_SAMPS 960 /* 60ms @ 16kHz, libopus's own max frame duration */

static volatile int running = 1;
static double gain = 1.0;
static int debug = 0;

static void on_signal(int sig) {
    (void) sig;
    running = 0;
}

static int cpld_ioctl(int num) {
    int fd = open(CPLD_DEV, O_RDWR);
    if (fd < 0) {
        fprintf(stderr, "talkback_rx: cannot open %s: %s\n", CPLD_DEV, strerror(errno));
        return -1;
    }
    ioctl(fd, _IOC(0, CPLD_DEVICE_NUM, num, 0x00), 0);
    close(fd);
    return 0;
}

static void speaker_set(int *state, int on) {
    if (*state == on) return;
    if (debug) fprintf(stderr, "talkback_rx: speaker %s\n", on ? "on" : "off");
    cpld_ioctl(on ? SPEAKER_ON_NUM : SPEAKER_OFF_NUM);
    *state = on;
}

/* (re)open the fifo for writing; rmm holds the read end open, so this should
 * return immediately rather than block. */
static int fifo_open(void) {
    int fd = open(FIFO_PATH, O_WRONLY | O_NONBLOCK);
    if (fd < 0) {
        fprintf(stderr, "talkback_rx: cannot open %s: %s\n", FIFO_PATH, strerror(errno));
        return -1;
    }
    /* clear O_NONBLOCK now a reader is confirmed present, so writes block
     * normally instead of returning EAGAIN under backpressure */
    int flags = fcntl(fd, F_GETFL, 0);
    fcntl(fd, F_SETFL, flags & ~O_NONBLOCK);
    return fd;
}

static long now_ms(void) {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return (long) tv.tv_sec * 1000 + tv.tv_usec / 1000;
}

/* Strip the RFC 3550 RTP header, including the optional extension block
 * WebRTC attaches; return the payload, or NULL if the packet is too short. */
static const unsigned char *rtp_strip_header(const unsigned char *pkt, int n, int *payload_len) {
    if (n < 12) return NULL;
    int cc = pkt[0] & 0x0F;
    int has_ext = (pkt[0] & 0x10) != 0;
    int off = 12 + cc * 4;
    if (off > n) return NULL;
    if (has_ext) {
        if (off + 4 > n) return NULL;
        int ext_len_words = (pkt[off + 2] << 8) | pkt[off + 3];
        off += 4 + ext_len_words * 4;
        if (off > n) return NULL;
    }
    *payload_len = n - off;
    return pkt + off;
}

int main(int argc, char **argv) {
    int c;
    while ((c = getopt(argc, argv, "g:dh")) != -1) {
        switch (c) {
        case 'g':
            gain = atof(optarg);
            break;
        case 'd':
            debug = 1;
            break;
        case 'h':
        default:
            fprintf(stderr, "Usage: %s [-g GAIN] [-d]\n", argv[0]);
            return c == 'h' ? 0 : 1;
        }
    }

    signal(SIGINT, on_signal);
    signal(SIGTERM, on_signal);
    signal(SIGPIPE, SIG_IGN);

    int sock = socket(AF_INET, SOCK_DGRAM, 0);
    if (sock < 0) {
        fprintf(stderr, "talkback_rx: socket: %s\n", strerror(errno));
        return 1;
    }
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_ANY);
    addr.sin_port = htons(UDP_PORT);
    if (bind(sock, (struct sockaddr *) &addr, sizeof(addr)) < 0) {
        fprintf(stderr, "talkback_rx: bind :%d: %s\n", UDP_PORT, strerror(errno));
        return 1;
    }
    /* recv timeout lets the main loop notice idle/signals without a thread */
    struct timeval rcvto = {0, 200 * 1000};
    setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &rcvto, sizeof(rcvto));

    NeAACDecHandle aacdec = NeAACDecOpen();
    if (!aacdec) {
        fprintf(stderr, "talkback_rx: NeAACDecOpen failed\n");
        return 1;
    }
    NeAACDecConfigurationPtr aaccfg = NeAACDecGetCurrentConfiguration(aacdec);
    aaccfg->defObjectType = LC;
    aaccfg->defSampleRate = 16000;
    aaccfg->outputFormat = FAAD_FMT_16BIT;
    aaccfg->dontUpSampleImplicitSBR = 1;  // keep the stream's real rate
    NeAACDecSetConfiguration(aacdec, aaccfg);
    int aac_init = 0;

    int opus_err = 0;
    OpusDecoder *opusdec = opus_decoder_create(OPUS_SAMPLE_RATE, 1, &opus_err);
    if (!opusdec || opus_err != OPUS_OK) {
        fprintf(stderr, "talkback_rx: opus_decoder_create failed: %d\n", opus_err);
        return 1;
    }

    int fifo_fd = -1;
    int speaker_state = 0;
    long last_pkt_ms = 0;
    int have_pkt_ever = 0;

    unsigned char inbuf[MAX_PKT];
    short pcmbuf[AAC_MAX_NSAMPS * AAC_MAX_NCHANS];
    short scaled[AAC_MAX_NSAMPS * AAC_MAX_NCHANS];
    short rsbuf[AAC_MAX_NSAMPS * 2];
    int logged_rate = 0;

    fprintf(stderr, "talkback_rx: listening on UDP :%d, gain=%.2f\n", UDP_PORT, gain);

    while (running) {
        int n = recvfrom(sock, inbuf, sizeof(inbuf), 0, NULL, NULL);

        long t = now_ms();

        if (n <= 0) {
            if (have_pkt_ever && speaker_state && (t - last_pkt_ms) > IDLE_TIMEOUT_MS) {
                speaker_set(&speaker_state, 0);
                if (fifo_fd >= 0) { close(fifo_fd); fifo_fd = -1; }
            }
            continue;
        }

        last_pkt_ms = t;
        have_pkt_ever = 1;

        if (!speaker_state) {
            speaker_set(&speaker_state, 1);
        }
        if (fifo_fd < 0) {
            fifo_fd = fifo_open();
            if (fifo_fd < 0) continue;
        }

        int nsamps = 0;
        short *play = pcmbuf;
        int is_rtp = (inbuf[0] & 0xC0) == 0x80;

        if (is_rtp) {
            int payload_len = 0;
            const unsigned char *payload = rtp_strip_header(inbuf, n, &payload_len);
            if (!payload || payload_len <= 0) {
                if (debug) fprintf(stderr, "talkback_rx: malformed RTP packet (n=%d)\n", n);
                continue;
            }
            int ret = opus_decode(opusdec, payload, payload_len, pcmbuf, OPUS_MAX_FRAME_SAMPS, 0);
            if (ret < 0) {
                if (debug) fprintf(stderr, "talkback_rx: opus_decode error %d (n=%d, payload_len=%d)\n", ret, n, payload_len);
                continue;
            }
            nsamps = ret; /* samples per channel (mono) */
        } else {
            const unsigned char *p = inbuf;
            int p_len = n;
            if (!aac_init) {
                unsigned long sr = 0;
                unsigned char ch = 0;
                int used = NeAACDecInit(aacdec, inbuf, (unsigned long)n, &sr, &ch);
                if (used < 0) {
                    if (debug) fprintf(stderr, "talkback_rx: NeAACDecInit failed (n=%d)\n", n);
                    continue;
                }
                aac_init = 1;
                p += used;
                p_len -= used;
            }
            if (p_len <= 0) continue;
            NeAACDecFrameInfo info;
            short *out = (short *) NeAACDecDecode(aacdec, &info,
                                                  (unsigned char *) p, p_len);
            if (out == NULL || info.error > 0) {
                if (debug) fprintf(stderr, "talkback_rx: AAC decode error %d (n=%d)\n",
                                   info.error, n);
                continue;
            }
            int ch = info.channels > 0 ? info.channels : 1;
            nsamps = (int)(info.samples / (unsigned int) ch);
            for (int i = 0; i < nsamps; i++) pcmbuf[i] = out[i * ch];
            if (!logged_rate) {
                fprintf(stderr, "talkback_rx: aac rate=%lu ch=%d sbr=%d obj=%d samples=%d\n",
                        info.samplerate, info.channels, info.sbr, info.object_type, nsamps);
                logged_rate = 1;
            }
            if (info.samplerate != 16000) {
                nsamps = resample_to_16k(pcmbuf, nsamps, info.samplerate,
                                         rsbuf, (int)(sizeof(rsbuf) / sizeof(rsbuf[0])));
                play = rsbuf;
            }
        }
        if (nsamps <= 0) continue;

        for (int i = 0; i < nsamps; i++) {
            double v = ((double) play[i]) * gain;
            if (v > 32767.0) v = 32767.0;
            if (v < -32768.0) v = -32768.0;
            scaled[i] = (short) v;
        }

        ssize_t towrite = (ssize_t) nsamps * sizeof(short);
        ssize_t written = write(fifo_fd, scaled, towrite);
        if (written < 0) {
            fprintf(stderr, "talkback_rx: fifo write failed: %s, reopening\n", strerror(errno));
            close(fifo_fd);
            fifo_fd = -1;
        }
    }

    if (speaker_state) speaker_set(&speaker_state, 0);
    if (fifo_fd >= 0) close(fifo_fd);
    NeAACDecClose(aacdec);
    opus_decoder_destroy(opusdec);
    close(sock);
    return 0;
}
