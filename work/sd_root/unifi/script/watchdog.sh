#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors

# UniFi Protect emulation -- process supervisor: every $WATCHDOG_INTERVAL seconds
# make sure exactly ONE of each project process runs (start the missing, kill
# the duplicates); the encoder has its own strike/reboot logic below.
#
#   watchdog.sh                          run the supervisor (singleton; a second
#                                        copy exits at once)
#   watchdog.sh restart bridge|avclient|talkback|all
#   watchdog.sh status
#
# Every start/stop/restart - the loop's and the CLI's - runs under one lock, so a
# manual restart can no longer race the loop into a second instance. Restarting
# the bridge also restarts the avclient: the client dedups CONNECT, so a fresh
# bridge is otherwise never re-driven and stays video-less.

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
MODEL_SUFFIX=$(cat "$UNIFI_PREFIX/etc/model_suffix" 2>/dev/null || echo y623)
CONF="$UNIFI_PREFIX/etc/unifi.cfg"
get_cfg() { grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-; }

INTERVAL=${WATCHDOG_INTERVAL:-$(get_cfg WATCHDOG_INTERVAL)}
[ -z "$INTERVAL" ] && INTERVAL=10
RESOLUTION=$(get_cfg RESOLUTION); [ -z "$RESOLUTION" ] && RESOLUTION=both
AUDIO=$(get_cfg AUDIO); [ -z "$AUDIO" ] && AUDIO=aac
YI_CLOUD=$(get_cfg YI_CLOUD)
if [ -z "$YI_CLOUD" ]; then
    # Default on, except on mediad builds (IS_MEDIAD=yes): mediad replaces the
    # stock encoder daemon and the Yi cloud is not run alongside it.
    if [ "$(get_cfg IS_MEDIAD)" = "yes" ]; then YI_CLOUD=no; else YI_CLOUD=yes; fi
fi

# PIDs whose argv[0] equals $1 or has the same basename (a substring grep would
# also match e.g. `cat /tmp/unifi_flv_bridge.log`). Zombies are skipped.
pids_of() {
    ps | awk -v n="$1" 'NR > 1 && $4 !~ /^Z/ { c = $5; b = c; sub(/.*\//, "", b); m = n; sub(/.*\//, "", m)
        if (c == n || b == m) print $1 }'
}
alive() { [ -n "$(pids_of "$1")" ]; }

log() { echo "$(date +'%H:%M:%S') $*"; }

# ---- lock: serialises every start/stop (loop and CLI) ----
# Lock files (atomic create via noclobber); the camera's busybox has no rmdir.
LOCKF=/tmp/unifi_wd.lk
try_create() { ( set -C; echo $$ > "$1" ) 2>/dev/null; }
lock_take() {
    _i=0
    while ! try_create "$LOCKF"; do
        _h=$(cat "$LOCKF" 2>/dev/null)
        if [ -n "$_h" ] && ! kill -0 "$_h" 2>/dev/null; then    # holder died
            rm -f "$LOCKF"; continue
        fi
        _i=$((_i+1))
        if [ "$_i" -gt 300 ]; then                              # ~60 s
            [ -z "$_h" ] && { rm -f "$LOCKF"; continue; }       # torn, ownerless
            return 1
        fi
        sleep 0.2
    done
}
lock_drop() { [ "$(cat "$LOCKF" 2>/dev/null)" = "$$" ] && rm -f "$LOCKF"; return 0; }

# ---- per-service primitives ----
proc_name() {
    case "$1" in
        bridge) echo unifi_flv_bridge ;;
        avclient) echo unifi_avclient_go ;;
        talkback) echo talkback_rx ;;
    esac
}

# Launch detached (the subshell exits at once, so init adopts and reaps it - no
# zombie children of this script), with a clean stdin.
start_svc() {
    case "$1" in
        bridge)
            ( cd "$UNIFI_PREFIX/bin" && exec ./unifi_flv_bridge -m "$MODEL_SUFFIX" -r "$RESOLUTION" -s -a "$AUDIO" \
                > /tmp/unifi_flv_bridge.log 2>&1 < /dev/null & ) ;;
        avclient)
            ( cd "$UNIFI_PREFIX/bin" && exec ./unifi_avclient_go \
                -cert "$UNIFI_PREFIX/etc/unifi_client_go.crt" \
                -key  "$UNIFI_PREFIX/etc/unifi_client_go.key" \
                > /tmp/avclient.log 2>&1 < /dev/null & ) ;;
        talkback)
            ( cd "$UNIFI_PREFIX/bin" && exec ./talkback_rx \
                > /tmp/talkback_rx.log 2>&1 < /dev/null & ) ;;
    esac
    log "started $1"
}

# TERM every instance, wait until gone, KILL what is left. Returns only once no
# instance remains, so a start right after it cannot overlap the old process.
stop_svc() {
    _n=$(proc_name "$1")
    _p=$(pids_of "$_n")
    [ -z "$_p" ] && return 0
    kill $_p 2>/dev/null
    _i=0
    while [ -n "$(pids_of "$_n")" ] && [ "$_i" -lt 15 ]; do sleep 0.2; _i=$((_i+1)); done
    _p=$(pids_of "$_n")
    if [ -n "$_p" ]; then
        log "$1 ignored TERM, killing: $_p"
        kill -9 $_p 2>/dev/null
        _i=0
        while [ -n "$(pids_of "$_n")" ] && [ "$_i" -lt 10 ]; do sleep 0.2; _i=$((_i+1)); done
    fi
}

# Exactly one instance: start if none, keep the oldest (lowest PID) and kill the
# rest if several. Sets STARTED_$1=1 when it (re)started the service.
ensure_svc() {
    _n=$(proc_name "$1")
    set -- "$1" $(pids_of "$_n")
    _svc=$1; shift
    if [ $# -eq 0 ]; then
        start_svc "$_svc"; return 1
    fi
    if [ $# -gt 1 ]; then
        _keep=$1
        for _x in "$@"; do [ "$_x" -lt "$_keep" ] && _keep=$_x; done
        log "DUPLICATE $_svc: pids $* - keeping $_keep"
        for _x in "$@"; do [ "$_x" != "$_keep" ] && kill -9 "$_x" 2>/dev/null; done
    fi
    return 0
}

# One supervision pass (caller holds the lock).
ensure_stack() {
    ensure_svc bridge; _b=$?
    ensure_svc avclient; _a=$?
    if [ "$_b" -eq 1 ] && [ "$_a" -eq 0 ]; then
        # Bridge came back but the client kept running: it will not re-send
        # CONNECT, so bounce it to re-drive the fresh bridge.
        log "bridge restarted - bouncing avclient to re-drive it"
        stop_svc avclient; start_svc avclient
    fi
    ensure_svc talkback
}

restart_cmd() {
    case "$1" in
        bridge)   stop_svc avclient; stop_svc bridge; start_svc bridge; start_svc avclient ;;
        avclient) stop_svc avclient; start_svc avclient ;;
        talkback) stop_svc talkback; start_svc talkback ;;
        all)      stop_svc avclient; stop_svc bridge; stop_svc talkback
                  start_svc bridge; start_svc avclient; start_svc talkback ;;
        *) echo "usage: $0 restart bridge|avclient|talkback|all" >&2; return 2 ;;
    esac
}

# ---- CLI ----
case "$1" in
    restart)
        lock_take || { echo "watchdog: lock busy" >&2; exit 1; }
        trap 'lock_drop' EXIT INT TERM
        restart_cmd "$2"; exit $?
        ;;
    status)
        for _s in bridge avclient talkback; do
            echo "$_s: $(pids_of "$(proc_name $_s)")"
        done
        exit 0
        ;;
esac

# ---- singleton: a second supervisor exits instead of double-starting ----
SINGLE=/tmp/unifi_wd.pid
if ! try_create "$SINGLE"; then
    _o=$(cat "$SINGLE" 2>/dev/null)
    if [ -n "$_o" ] && [ "$_o" != "$$" ] && grep -q watchdog "/proc/$_o/cmdline" 2>/dev/null; then
        echo "watchdog already running (pid $_o)"; exit 0
    fi
    echo $$ > "$SINGLE"     # stale (owner gone / pid reused)
fi
trap '[ "$(cat "$SINGLE" 2>/dev/null)" = "$$" ] && rm -f "$SINGLE"; lock_drop' EXIT INT TERM

RMM_FAILS=0
MEDIAD_RESTARTS=0
while true; do
    sleep "$INTERVAL"
    lock_take || { log "lock busy, skipping pass"; continue; }

    # Encoder: mediad (drop-in) if opted in/installed, else stock rmm. Match the
    # daemon binary path, not "mediad" (mediad.sh's launcher line also contains it).
    IS_MEDIAD=$(get_cfg IS_MEDIAD); [ -z "$IS_MEDIAD" ] && IS_MEDIAD=no
    if [ "$IS_MEDIAD" = "yes" ] && [ -x "$UNIFI_PREFIX/bin/mediad" ] && [ -x "$UNIFI_PREFIX/script/mediad.sh" ]; then
        if alive "$UNIFI_PREFIX/bin/mediad"; then
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

    ensure_stack

    # Yi cloud daemons (YI_CLOUD=yes only). Restart only the one that died, so a
    # single failure does not spawn duplicate P2P/storage sessions.
    if [ "$YI_CLOUD" = "yes" ]; then
        alive 'p2p_tnp' || ( cd /home/app; ./p2p_tnp >/dev/null 2>&1 & )
        alive 'oss'     || ( cd /home/app; ./oss     >/dev/null 2>&1 & )
        alive 'cloud'   || ( cd /home/app; ./cloud   >/dev/null 2>&1 & )
    fi
    lock_drop
done
