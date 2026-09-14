#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# detect-model: work out which camera we are running on, from the OS alone.
# Prints the model and records it in $UNIFI_PREFIX/etc/model_suffix.
#
# Sources, in order:
#   1. $1 / $SUFFIX -- /backup/init.sh sets this stock model token before
#      sourcing us (it also names home_${SUFFIX}m updates).
#   2. MIPI sensor driver /sys/module/<sensor>_mipi, e.g. gc3003_mipi.
#   3. /backup/upgrade_conf OTA URL (.../familymonitor-<model>/).
#   4. fallback default y623.

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
OUT="$UNIFI_PREFIX/etc/model_suffix"

model="$1"
[ -z "$model" ] && model="$SUFFIX"

# 2. sensor driver -> model (only unambiguous mappings; $SUFFIX is preferred)
if [ -z "$model" ]; then
    for k in /sys/module/*_mipi; do
        [ -e "$k" ] || continue
        s=${k##*/}        # gc3003_mipi
        s=${s%_mipi}      # gc3003
        case "$s" in
            gc3003) model=y623 ;;
            gc2053) model=h52ga ;;
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
