#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors

# UniFi Protect emulation -- process watchdog.
# Polls every $WATCHDOG_INTERVAL seconds and restarts any dead project process.
# The encoder is watched too: stock rmm gets 5 strikes then a reboot (a dead rmm
# can't be restarted); mediad is restarted, and reboots only if 3 restarts fail
# (the box otherwise sits "online" with no video).

UNIFI_PREFIX="/tmp/sd/unifi"
MODEL_SUFFIX=$(cat "$UNIFI_PREFIX/etc/model_suffix" 2>/dev/null || echo y623)
CONF="$UNIFI_PREFIX/etc/unifi.cfg"
get_cfg() { grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-; }

INTERVAL=${WATCHDOG_INTERVAL:-$(get_cfg WATCHDOG_INTERVAL)}
[ -z "$INTERVAL" ] && INTERVAL=10
RESOLUTION=$(get_cfg RESOLUTION); [ -z "$RESOLUTION" ] && RESOLUTION=both
AUDIO=$(get_cfg AUDIO); [ -z "$AUDIO" ] && AUDIO=aac
# PTZ default by model, as in init.sh; without it a watchdog restart dropped
# -ptz and Protect lost the PTZ controls until reboot.
PTZ=$(get_cfg PTZ)
if [ -z "$PTZ" ]; then
    case "$MODEL_SUFFIX" in
        r30gb|r35gb|r37gb|r40ga|q321br_lsx|qg311r|b091qp|h30ga|h51ga|h52ga|h60ga)
            PTZ=yes ;;
        *) PTZ=no ;;
    esac
fi

alive() {
    ps | grep -v grep | grep -q "$1"
}

log() { echo "$(date +'%H:%M:%S') $*"; }

restart_flv_bridge() {
    killall -q unifi_flv_bridge
    sleep 1
    cd "$UNIFI_PREFIX/bin"
    ./unifi_flv_bridge -m "$MODEL_SUFFIX" -r "$RESOLUTION" -s -a "$AUDIO" > /tmp/unifi_flv_bridge.log 2>&1 &
}

restart_avclient() {
    killall -q unifi_avclient_go
    sleep 1
    cd "$UNIFI_PREFIX/bin"
    PTZ_OPT=""
    [ "$PTZ" = "yes" ] && PTZ_OPT="-ptz"
    ./unifi_avclient_go $PTZ_OPT \
        -cert "$UNIFI_PREFIX/etc/unifi_client_go.crt" \
        -key  "$UNIFI_PREFIX/etc/unifi_client_go.key" \
        > /tmp/avclient.log 2>&1 &
}

restart_talkback() {
    killall -q talkback_rx
    sleep 1
    cd "$UNIFI_PREFIX/bin"
    ./talkback_rx > /tmp/talkback_rx.log 2>&1 &
}

RMM_FAILS=0
MEDIAD_RESTARTS=0
while true; do
    sleep "$INTERVAL"

    # video encoder: mediad (drop-in) when opted in and installed, else stock
    # rmm. mediad can be restarted; stock rmm cannot, so its path reboots. A
    # missing mediad is restarted after 5 checks, and the box reboots if
    # restarts don't bring it back (a running-but-wedged encoder shows no video).
    IS_MEDIAD=$(get_cfg IS_MEDIAD); [ -z "$IS_MEDIAD" ] && IS_MEDIAD=no
    if [ "$IS_MEDIAD" = "yes" ] && [ -x "$UNIFI_PREFIX/bin/mediad" ] && [ -x "$UNIFI_PREFIX/script/mediad.sh" ]; then
        if alive 'mediad'; then
            [ "$RMM_FAILS" -ne 0 ] && log "mediad present again (was $RMM_FAILS fails)"
            RMM_FAILS=0
            MEDIAD_RESTARTS=0
        else
            RMM_FAILS=$((RMM_FAILS+1))
            log "mediad MISSING ($RMM_FAILS/5)"
            if [ "$RMM_FAILS" -ge 5 ]; then
                MEDIAD_RESTARTS=$((MEDIAD_RESTARTS+1))
                if [ "$MEDIAD_RESTARTS" -le 3 ]; then
                    log "restarting mediad (attempt $MEDIAD_RESTARTS/3)"
                    "$UNIFI_PREFIX/script/mediad.sh" restart
                    RMM_FAILS=0
                else
                    log "REBOOT: mediad missing after 3 restarts"
                    /sbin/reboot
                fi
            fi
        fi
    elif alive './rmm'; then
        [ "$RMM_FAILS" -ne 0 ] && log "encoder present again (was $RMM_FAILS fails)"
        RMM_FAILS=0
    else
        RMM_FAILS=$((RMM_FAILS+1))
        log "encoder MISSING (rmm) ($RMM_FAILS/5)"
        if [ "$RMM_FAILS" -ge 5 ]; then
            log "REBOOT: encoder missing 5 consecutive checks"
            /sbin/reboot
        fi
    fi

    alive 'unifi_flv_bridge' || restart_flv_bridge
    alive 'unifi_avclient_go' || restart_avclient
    alive 'talkback_rx' || restart_talkback
done
