/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Per-model facts are data, not code: read from unifi/etc/model_table. This
 * file only knows how to find a model's row; it contains no model names or
 * per-model branches.
 */
#include "bridge.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <strings.h>

namespace {

// Same path the boot scripts use; UNIFI_PREFIX may be overridden for tests.
const char *kDefaultTablePath = "/tmp/sd/unifi/etc/model_table";

// Conservative fallback for a missing table or an unlisted model. Matches the
// historical defaults (most families are 368/28; y623-class is 2304x1296).
const ModelParams kFallback = {368, 28, 2304, 1296, false};

}  // namespace

ModelParams modelParams(const char *name) {
    if (name == nullptr || *name == '\0') return kFallback;

    const char *prefix = getenv("UNIFI_PREFIX");
    char path[256];
    const char *table = getenv("UNIFI_MODEL_TABLE");
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
        std::fprintf(stderr, "unifi_flv_bridge: model table %s not readable; "
                             "using defaults (368/28, 2304x1296, no PTZ)\n", table);
        return kFallback;
    }

    char line[256];
    while (std::fgets(line, sizeof(line), f)) {
        if (char *hash = std::strchr(line, '#')) *hash = '\0';
        char m[64] = {0}, sensor[64] = {0}, ptz[16] = {0};
        unsigned off = 0, hdr = 0, w = 0, h = 0;
        int n = std::sscanf(line, "%63s %63s %u %u %u %u %15s",
                            m, sensor, &off, &hdr, &w, &h, ptz);
        if (n < 7) continue;                 // comment/blank/partial line
        if (strcasecmp(m, name) != 0) continue;

        ModelParams out;
        out.offset = off;
        out.headerSize = (int)hdr;
        out.highWidth = w;
        out.highHeight = h;
        out.ptz = (strcasecmp(ptz, "yes") == 0);
        std::fclose(f);
        return out;
    }
    std::fclose(f);

    std::fprintf(stderr, "unifi_flv_bridge: model %s not in %s; using defaults "
                         "(368/28, 2304x1296, no PTZ)\n", name, table);
    return kFallback;
}
