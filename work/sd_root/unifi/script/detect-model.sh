#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# detect-model: work out which camera we are running on, from the OS alone.
# Prints the model and records it in $UNIFI_PREFIX/etc/model_suffix.
#
# Sources, in order:
#   1. $1 / $SUFFIX -- /backup/init.sh sets this stock model token before
#      sourcing us (it also names home_${SUFFIX}m updates).
#   2. MIPI sensor driver -- ONLY for a sensor unique to one model (gc3003).
#      gc2053 is shared by h52ga and r35gb, so it must NOT pick a model here:
#      mediad keys mounting orientation on the model, and a wrong model would
#      leave an r35gb's 180-degree-rotated sensor unflipped (upside-down image).
#   3. /backup/upgrade_conf OTA URL (.../familymonitor-<model>/).
#   4. fallback default y623.

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
OUT="$UNIFI_PREFIX/etc/model_suffix"

model="$1"
[ -z "$model" ] && model="$SUFFIX"

# 2. sensor driver -> model, only for a sensor unique to ONE model. gc2053 is
#    shared by h52ga and r35gb, so mapping it here can mislabel an r35gb as
#    h52ga (mediad then skips r35gb's mirror+flip and the image is upside-down).
#    Ambiguous sensors stay unmapped; step 3 (OTA URL) or the y623 default wins.
if [ -z "$model" ]; then
    for k in /sys/module/*_mipi; do
        [ -e "$k" ] || continue
        s=${k##*/}        # gc3003_mipi
        s=${s%_mipi}      # gc3003
        case "$s" in
            gc3003) model=y623 ;;
            # gc2053: ambiguous (h52ga, r35gb) - leave unset
        esac
        [ -n "$model" ] && break
    done
fi

# 3. stock OTA config URL
if [ -z "$model" ] && [ -f /backup/upgrade_conf ]; then
    model=$(grep -ao "familymonitor-[a-zA-Z0-9_]*" /backup/upgrade_conf 2>/dev/null \
            | sed 's/.*familymonitor-//' | sed -n 1p)
fi

# 4. project reference default
[ -z "$model" ] && model=y623

if [ -d "$UNIFI_PREFIX/etc" ]; then
    printf '%s\n' "$model" > "$OUT" 2>/dev/null
fi
printf '%s\n' "$model"
