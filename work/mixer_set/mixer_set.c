// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors
/* mixer_set <card> <control> [0-100]: print or set an ALSA capture volume as
 * raw = min + vol*(max-min)/100, as UniFi's pcm_mixer_wrapper_t::SetVolume does
 * (e.g. "MIC1 gain volume" on hw:0), so AAC and Opus scale together.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <alsa/asoundlib.h>

int main(int argc, char **argv)
{
    if (argc != 3 && argc != 4) {
        fprintf(stderr, "usage: %s <card> <control> [0-100]\n", argv[0]);
        return 2;
    }
    const char *card = argv[1];
    const char *name = argv[2];

    snd_mixer_t *m = NULL;
    if (snd_mixer_open(&m, 0) < 0) {
        fprintf(stderr, "mixer_set: snd_mixer_open failed\n");
        return 1;
    }
    if (snd_mixer_attach(m, card) < 0) {
        fprintf(stderr, "mixer_set: attach %s failed\n", card);
        snd_mixer_close(m);
        return 1;
    }
    snd_mixer_selem_register(m, NULL, NULL);
    if (snd_mixer_load(m) < 0) {
        fprintf(stderr, "mixer_set: snd_mixer_load failed\n");
        snd_mixer_close(m);
        return 1;
    }

    snd_mixer_elem_t *e = NULL;
    for (snd_mixer_elem_t *it = snd_mixer_first_elem(m); it; it = snd_mixer_elem_next(it)) {
        if (strcmp(snd_mixer_selem_get_name(it), name) == 0) {
            e = it;
            break;
        }
    }
    if (!e) {
        fprintf(stderr, "mixer_set: control '%s' not found on %s\n", name, card);
        snd_mixer_close(m);
        return 1;
    }

    int capture = snd_mixer_selem_has_capture_volume(e);
    long min = 0, max = 0;
    if (capture) {
        snd_mixer_selem_get_capture_volume_range(e, &min, &max);
    } else if (snd_mixer_selem_has_playback_volume(e)) {
        snd_mixer_selem_get_playback_volume_range(e, &min, &max);
    } else {
        fprintf(stderr, "mixer_set: control '%s' has no volume\n", name);
        snd_mixer_close(m);
        return 1;
    }

    if (argc == 3) {
        long cur = 0;
        if (capture) snd_mixer_selem_get_capture_volume(e, 0, &cur);
        else         snd_mixer_selem_get_playback_volume(e, 0, &cur);
        printf("mixer_set: %s|%s min %ld max %ld cur %ld\n", card, name, min, max, cur);
        snd_mixer_close(m);
        return 0;
    }

    long pct = strtol(argv[3], NULL, 10);
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;

    long raw = min + (long)(pct * (max - min)) / 100;
    if (raw < min) raw = min;
    if (raw > max) raw = max;

    int rc = capture ? snd_mixer_selem_set_capture_volume_all(e, raw)
                     : snd_mixer_selem_set_playback_volume_all(e, raw);
    if (rc < 0) {
        fprintf(stderr, "mixer_set: set failed: %s\n", snd_strerror(rc));
        snd_mixer_close(m);
        return 1;
    }

    printf("mixer_set: %s|%s %ld%% -> %ld (min %ld max %ld)\n",
           card, name, pct, raw, min, max);
    snd_mixer_close(m);
    return 0;
}
