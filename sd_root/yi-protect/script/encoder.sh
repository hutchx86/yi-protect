#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Encoder selection -- the single source of truth for which encoder the boot
# scripts run. Sourced by init.sh and watchdog.sh (so they can never disagree);
# also runnable directly to print the resolved choice.
#
# yi-protect.cfg IS_MEDIAD:
#   yes       force mediad (falls back to rmm if it is not fully installed)
#   no        force the stock rmm
#   auto, ""  mediad when fully installed, else rmm   (default)
#
# "Fully installed" = bin/mediad AND script/mediad.sh AND lib/libvenc_base.so
# (mediad resolves libvenc_base.so via its $ORIGIN/../lib rpath at load time).
#
# encoder_resolve sets:
#   ENCODER=mediad|rmm
#   ENCODER_REASON   one-line explanation, for logging/status

encoder_resolve() {
    _p="${YIP_PREFIX:-/tmp/sd/yi-protect}"
    _want=auto
    if [ -f "$_p/etc/yi-protect.cfg" ]; then
        _want=$(sed -n 's/^IS_MEDIAD=//p' "$_p/etc/yi-protect.cfg" 2>/dev/null | tail -1)
        _want=$(printf '%s' "$_want" | tr -d "[:space:]'\"")
        [ -n "$_want" ] || _want=auto
    fi
    if [ -x "$_p/bin/mediad" ] && [ -x "$_p/script/mediad.sh" ] && [ -f "$_p/lib/libvenc_base.so" ]; then
        _installed=yes
    else
        _installed=no
    fi
    case "$_want" in
        no)  ENCODER=rmm
             ENCODER_REASON="rmm (IS_MEDIAD=no)" ;;
        yes) if [ "$_installed" = yes ]; then
                 ENCODER=mediad; ENCODER_REASON="mediad (IS_MEDIAD=yes)"
             else
                 ENCODER=rmm;    ENCODER_REASON="rmm (IS_MEDIAD=yes, mediad not fully installed)"
             fi ;;
        *)   if [ "$_installed" = yes ]; then
                 ENCODER=mediad; ENCODER_REASON="mediad (auto: installed)"
             else
                 ENCODER=rmm;    ENCODER_REASON="rmm (auto: mediad not installed)"
             fi ;;
    esac
    return 0
}

# Executed (not sourced): print the resolution, for `watchdog.sh status` and
# manual inspection.
case "$0" in
    */encoder.sh|encoder.sh)
        encoder_resolve
        echo "$ENCODER: $ENCODER_REASON" ;;
esac
