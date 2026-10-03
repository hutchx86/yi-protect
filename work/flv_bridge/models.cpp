/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Per-model facts are data, not code: read from yi-protect/etc/model_table. This
 * file only knows how to find a model's row; it contains no model names or
 * per-model branches.
 */
#include "bridge.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <strings.h>

namespace {

// Same path the boot scripts use; YIP_PREFIX may be overridden for tests.
const char *kDefaultTablePath = "/tmp/sd/yi-protect/etc/model_table";

// Conservative fallback for a missing table or an unlisted model. Ring
// offset/header 0 = autodetect (FshareReader falls back to 368/28 if it cannot
// find the marker); geometry stays the historical y623-class default.
const unsigned kDefaultHighBitrate = 2000000;
const ModelParams kFallback = {0, 0, 2304, 1296, false, kDefaultHighBitrate};

}  // namespace

ModelParams modelParams(const char *name) {
    if (name == nullptr || *name == '\0') return kFallback;

    const char *prefix = getenv("YIP_PREFIX");
    char path[256];
    const char *table = getenv("YIP_MODEL_TABLE");
    if (table == nullptr || *table == '\0') {
        if (prefix != nullptr && *prefix != '\0') {
            std::snprintf(path, sizeof(path), "%s/etc/model_table", prefix);
            table = path;
        } else {
            table = kDefaultTablePath;
        }
    }

    FILE *f = std::fopen(table, "r");
    if (f == nullptr) {
        std::fprintf(stderr, "yi_protect_flv_bridge: model table %s not readable; "
                             "using defaults (autodetect ring, 2304x1296, no PTZ)\n", table);
        return kFallback;
    }

    char line[256];
    while (std::fgets(line, sizeof(line), f)) {
        if (char *hash = std::strchr(line, '#')) *hash = '\0';
        char m[64] = {0}, sensor[64] = {0}, ptz[16] = {0};
        unsigned off = 0, hdr = 0, w = 0, h = 0, highBitrate = 0, mw = 0, mh = 0;
        int n = std::sscanf(line, "%63s %63s %u %u %u %u %15s %u %u %u",
                            m, sensor, &off, &hdr, &w, &h, ptz, &highBitrate, &mw, &mh);
        if (n < 7) continue;                 // comment/blank/partial line
        if (strcasecmp(m, name) != 0) continue;

        ModelParams out;
        out.offset = off;
        out.headerSize = (int)hdr;
        // Declare the geometry the ACTIVE encoder actually streams: the model
        // row's mediad_w/h when the boot scripts selected the mediad path
        // (YIP_ENCODER=mediad), else the stock-rmm high_w/h. Missing mediad
        // columns fall back to high_w/h, so old cards keep working.
        const char *enc = std::getenv("YIP_ENCODER");
        bool mediadPath = (enc != nullptr && strcasecmp(enc, "mediad") == 0);
        if (mediadPath && n >= 10 && mw > 0 && mh > 0) {
            out.highWidth = mw;
            out.highHeight = mh;
        } else {
            out.highWidth = w;
            out.highHeight = h;
        }
        out.ptz = (strcasecmp(ptz, "yes") == 0);
        // The 8th column is optional; absent/0 means "follow the controller".
        out.highBitrate = (n >= 8 && highBitrate > 0) ? highBitrate : kDefaultHighBitrate;
        std::fclose(f);
        return out;
    }
    std::fclose(f);

    std::fprintf(stderr, "yi_protect_flv_bridge: model %s not in %s; using defaults "
                         "(autodetect ring, 2304x1296, no PTZ)\n", name, table);
    return kFallback;
}
