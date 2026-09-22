#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# detect-model: resolve the camera model from the OS alone and record it in
# $UNIFI_PREFIX/etc/model_suffix. Sources: $1/$SUFFIX, unique MIPI sensor, OTA URL, default y623.

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
OUT="$UNIFI_PREFIX/etc/model_suffix"
TABLE="$UNIFI_PREFIX/etc/model_table"

model="$1"
[ -z "$model" ] && model="$SUFFIX"

# 2. sensor driver -> model, but only when a single table row claims the sensor.
if [ -z "$model" ]; then
    for k in /sys/module/*_mipi; do
        [ -e "$k" ] || continue
        s=${k##*/}        # gc3003_mipi
        s=${s%_mipi}      # gc3003
        hits=$(grep -vE '^[[:space:]]*#|^[[:space:]]*$' "$TABLE" 2>/dev/null \
               | awk -v s="$s" '$2 == s {print $1}')
        n=$(printf '%s\n' "$hits" | grep -c .)
        if [ "$n" = "1" ]; then
            model=$hits
            break
        fi
    done
fi

if [ -z "$model" ] && [ -f /backup/upgrade_conf ]; then
    model=$(grep -ao "familymonitor-[a-zA-Z0-9_]*" /backup/upgrade_conf 2>/dev/null \
            | sed 's/.*familymonitor-//' | sed -n 1p)
fi

[ -z "$model" ] && model=y623

if [ -d "$UNIFI_PREFIX/etc" ]; then
    printf '%s\n' "$model" > "$OUT" 2>/dev/null
fi

# A model with no table row is worth seeing: general code then falls back to
# conservative defaults instead of that model's real facts.
if ! grep -vE '^[[:space:]]*#|^[[:space:]]*$' "$TABLE" 2>/dev/null \
     | awk -v m="$model" '$1 == m {found=1} END {exit !found}'; then
    echo "detect-model: WARNING: model '$model' has no row in $TABLE; using defaults" >&2
fi

printf '%s\n' "$model"
