#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# UniFi Protect emulation -- boot entry (SD override).
#
# /backup/init.sh sources this. It resolves the model (detect-model.sh), then
# builds this boot's bring-up from the camera's own stock lower_half_init.sh
# (gen-lower-half.sh: stock hardware bring-up, our app launch) and sources it.
# If the stock script is not in a shape we recognise, the stock script runs
# unmodified: the camera boots, without our app.

UNIFI_PREFIX=/tmp/sd/unifi
LH_LOG=/tmp/lower_half.log

# $SUFFIX is the stock ROM's model token; detect-model prefers it, else hardware.
MODEL=$("$UNIFI_PREFIX/script/detect-model.sh" "$SUFFIX")
SUFFIX="$MODEL"
export SUFFIX

# Same preference as the stock /backup/init.sh.
STOCK_LH=/home/app/lower_half_init.sh
[ -f "$STOCK_LH" ] || STOCK_LH=/backup/lower_half_init.sh

GEN_LH=/tmp/lower_half_init.sh
if sh "$UNIFI_PREFIX/script/gen-lower-half.sh" "$STOCK_LH" "$GEN_LH"; then
    echo "lower_half: $MODEL: generated from $STOCK_LH" >> "$LH_LOG"
    # Reference copy for debugging only; never read back at boot.
    cmp -s "$GEN_LH" "$UNIFI_PREFIX/log/lower_half_init.generated.sh" 2>/dev/null ||
        { mkdir -p "$UNIFI_PREFIX/log" && cp "$GEN_LH" "$UNIFI_PREFIX/log/lower_half_init.generated.sh"; } 2>/dev/null
    . "$GEN_LH"
else
    echo "lower_half: $MODEL: $STOCK_LH not recognised; running it unmodified (no yi-protect app)" >> "$LH_LOG"
    . "$STOCK_LH"
fi
