#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# UniFi Protect emulation -- boot entry (SD override).
#
# /backup/init.sh sources this; it resolves the model (detect-model.sh) and runs
# the matching vendored bring-up (lower_half/<model>.sh), starting script/init.sh.

UNIFI_PREFIX=/tmp/sd/unifi

# $SUFFIX is the stock ROM's model token; detect-model prefers it, else hardware.
MODEL=$("$UNIFI_PREFIX/script/detect-model.sh" "$SUFFIX")
SUFFIX="$MODEL"
export SUFFIX

LH="$UNIFI_PREFIX/script/lower_half/$MODEL.sh"
[ -f "$LH" ] || LH="$UNIFI_PREFIX/script/lower_half/default.sh"
if [ ! -f "$LH" ]; then
    # Last resort: the stock script, so the camera still boots (without our app).
    LH=/backup/lower_half_init.sh
fi

. "$LH"
