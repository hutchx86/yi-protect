#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# UniFi Protect emulation -- boot entry (SD override).
#
# /backup/init.sh sources this when present. It detects the camera model, runs
# the matching vendored bring-up (lower_half/<model>.sh), which starts
# unifi/script/init.sh. The model is resolved at boot (detect-model.sh), so one
# SD image boots any supported model.

UNIFI_PREFIX=/tmp/sd/unifi

# $SUFFIX is the stock ROM's model token (set by /backup/init.sh); detect-model
# prefers it and falls back to hardware.
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
