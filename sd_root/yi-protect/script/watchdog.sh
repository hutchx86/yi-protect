#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors

# UniFi Protect emulation -- process supervisor: every $WATCHDOG_INTERVAL seconds
# make sure exactly ONE of each project process runs (start the missing, kill
# the duplicates); the encoder has its own strike/reboot logic below.
#
#   watchdog.sh                          run the supervisor (singleton; a second
#                                        copy exits at once unless the running
#                                        one is stalled - see the heartbeat)
#   watchdog.sh restart bridge|avclient|talkback|all
#   watchdog.sh status
#
# Every start/stop/restart - the loop's and the CLI's - runs under one lock, so a
# manual restart can no longer race the loop into a second instance. Restarting
# the bridge also restarts the avclient: the client dedups CONNECT, so a fresh
# bridge is otherwise never re-driven and stays video-less.
#
# Liveness: the loop writes /proc/uptime seconds to the heartbeat file at the
# top of (and during) every pass. A starting watchdog replaces a singleton whose
# heartbeat has not moved for STALE seconds instead of exiting "already running"
# forever: a shell stuck mid-pass used to survive until the next reboot and took
# the whole recovery path down with it (h52ga, 2026-10-16).

YIP_PREFIX="${YIP_PREFIX:-/tmp/sd/yi-protect}"
# Regenerate model_suffix if it is missing/empty (see init.sh), so a restart the
# watchdog drives never uses the wrong model.
MODEL_SUFFIX=$(cat "$YIP_PREFIX/etc/model_suffix" 2>/dev/null)
if [ -z "$MODEL_SUFFIX" ] && [ -x "$YIP_PREFIX/script/detect-model.sh" ]; then
    MODEL_SUFFIX=$("$YIP_PREFIX/script/detect-model.sh")
    sync 2>/dev/null
fi
[ -z "$MODEL_SUFFIX" ] && MODEL_SUFFIX=y623
CONF="$YIP_PREFIX/etc/yi-protect.cfg"
get_cfg() { grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-; }

# Encoder path (shared with init.sh): resolve once. YIP_ENCODER is exported so
# the bridge/client the watchdog (re)starts declare the matching model_table
# geometry. A missing helper (partial card) -> rmm.
if [ -r "$YIP_PREFIX/script/encoder.sh" ]; then
    . "$YIP_PREFIX/script/encoder.sh"
    encoder_resolve
else
    ENCODER=rmm; ENCODER_REASON="rmm (encoder.sh missing)"
fi
export YIP_ENCODER="$ENCODER"

INTERVAL=${WATCHDOG_INTERVAL:-$(get_cfg WATCHDOG_INTERVAL)}
[ -z "$INTERVAL" ] && INTERVAL=10
RESOLUTION=$(get_cfg RESOLUTION); [ -z "$RESOLUTION" ] && RESOLUTION=both
AUDIO=$(get_cfg AUDIO); [ -z "$AUDIO" ] && AUDIO=aac
YI_CLOUD=$(get_cfg YI_CLOUD)
if [ -z "$YI_CLOUD" ]; then
    # Default on, except when the mediad encoder is active: mediad replaces the
    # stock encoder daemon and the Yi cloud is not run alongside it.
    if [ "$ENCODER" = "mediad" ]; then YI_CLOUD=no; else YI_CLOUD=yes; fi
fi

# PIDs whose argv[0] equals $1 or has the same basename (a substring grep would
# also match e.g. `cat /tmp/yi_protect_flv_bridge.log`). Zombies are skipped.
pids_of() {
    ps | awk -v n="$1" 'NR > 1 && $4 !~ /^Z/ { c = $5; b = c; sub(/.*\//, "", b); m = n; sub(/.*\//, "", m)
        if (c == n || b == m) print $1 }'
}
alive() { [ -n "$(pids_of "$1")" ]; }

log() { beat; echo "$(date +'%H:%M:%S') $*"; }

# ---- heartbeat ----
# /proc/uptime (integer seconds) is monotonic across NTP steps; busybox has no
# `stat`, and a heartbeat written by the looping shell also proves the shell is
# past any blocking point in the previous pass.
SINGLE=/tmp/yi_protect_wd.pid
BEAT=/tmp/yi_protect_wd.beat
STALE=$((INTERVAL * 3 + 120))       # 150 s at the default 10 s interval
now_up() { cut -d. -f1 /proc/uptime 2>/dev/null; }
beat() { now_up > "$BEAT" 2>/dev/null; }
beat_age() {
    _b=$(cat "$BEAT" 2>/dev/null)
    case "$_b" in ''|*[!0-9]*) echo 999999; return 0 ;; esac
    _n=$(now_up)
    case "$_n" in ''|*[!0-9]*) echo 999999; return 0 ;; esac
    echo $((_n - _b))
}

# ---- log trimming: /tmp is a 32 MB tmpfs ----
# The bridge's per-second queue-poll lines filled /tmp over ~13 days and took the
# whole stack with it (avclient.log no longer writable, service starts failing
# with ENOSPC). Keep each service log bounded: copy it to the card, then truncate
# in place. The services open these logs with >> (O_APPEND), so the writer's next
# write lands at offset 0 instead of after a hole, and the history survives a
# restart (the cap, not a restart, is what bounds the file).
LOG_CAP=${WATCHDOG_LOG_CAP:-2097152}    # 2 MB
trim_log() {
    _f=$1
    [ -f "$_f" ] || return 0
    _sz=$(ls -l "$_f" 2>/dev/null | awk '{print $5}')
    case "$_sz" in ''|*[!0-9]*) return 0 ;; esac
    [ "$_sz" -le "$LOG_CAP" ] && return 0
    _dir=/tmp/sd/wd-archived
    mkdir -p "$_dir" 2>/dev/null
    _keep="$_dir/$(basename "$_f")"
    cp -f "$_f" "$_keep" 2>/dev/null
    : > "$_f"
    log "trim_log: $_f reached ${_sz}B; archived to $_keep"
}
trim_logs() {
    trim_log /tmp/yi_protect_flv_bridge.log
    trim_log /tmp/avclient.log
    trim_log /tmp/talkback_rx.log
    trim_log /tmp/watchdog.log
}

# ---- lock: serialises every start/stop (loop and CLI) ----
# Lock files (atomic create via noclobber); the camera's busybox has no rmdir.
LOCKF=/tmp/yi_protect_wd.lk
watchdog_pid() {    # $1: pid that is (still) our script, not a reused PID
    [ -n "$1" ] && [ -d "/proc/$1" ] || return 1
    # argv[1] is the script path ("sh /path/watchdog.sh"); matching the second
    # word only avoids treating an unrelated `cat`/`tail` of the script as the
    # supervisor (the old whole-cmdline grep did).
    _a1=$(tr '\0' '\n' < "/proc/$1/cmdline" 2>/dev/null | sed -n '2p')
    case "$_a1" in
        */watchdog.sh|watchdog.sh) return 0 ;;
    esac
    return 1
}
lock_take() {
    _t=0
    # 600 * 0.2 s = 120 s: a pass that restarts mediad can legitimately hold it
    # for the 60 s startup window.
    while [ "$_t" -lt 600 ]; do
        if ( set -C; echo $$ > "$LOCKF" ) 2>/dev/null; then return 0; fi
        _h=$(cat "$LOCKF" 2>/dev/null)
        if [ -z "$_h" ]; then
            # Torn/ownerless (a create whose write failed) or a writer between
            # open and write: give it a moment, then reclaim deterministically
            # instead of spinning for the old 60 s.
            [ "$_t" -ge 3 ] && rm -f "$LOCKF"
        elif [ "$_h" = "$$" ]; then
            return 0
        elif ! watchdog_pid "$_h"; then
            rm -f "$LOCKF"                 # dead holder, or the PID was reused
        fi
        _t=$((_t+1))
        sleep 0.2
    done
    return 1
}
lock_drop() { [ "$(cat "$LOCKF" 2>/dev/null)" = "$$" ] && rm -f "$LOCKF"; return 0; }

# ---- per-service primitives ----
proc_name() {
    case "$1" in
        bridge) echo yi_protect_flv_bridge ;;
        avclient) echo yi_protect_avclient_go ;;
        talkback) echo talkback_rx ;;
    esac
}

# Launch detached (the subshell exits at once, so init adopts and reaps it - no
# zombie children of this script), with a clean stdin.
start_svc() {
    case "$1" in
        bridge)
            ( cd "$YIP_PREFIX/bin" && exec ./yi_protect_flv_bridge -m "$MODEL_SUFFIX" -r "$RESOLUTION" -a "$AUDIO" \
                >> /tmp/yi_protect_flv_bridge.log 2>&1 < /dev/null & ) ;;
        avclient)
            ( cd "$YIP_PREFIX/bin" && exec ./yi_protect_avclient_go \
                -cert "$YIP_PREFIX/etc/yi_protect_client_go.crt" \
                -key  "$YIP_PREFIX/etc/yi_protect_client_go.key" \
                >> /tmp/avclient.log 2>&1 < /dev/null & ) ;;
        talkback)
            ( cd "$YIP_PREFIX/bin" && exec ./talkback_rx \
                >> /tmp/talkback_rx.log 2>&1 < /dev/null & ) ;;
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
        trap 'lock_drop' EXIT
        trap 'exit 143' INT TERM
        restart_cmd "$2"; exit $?
        ;;
    status)
        echo "encoder: $ENCODER_REASON"
        for _s in bridge avclient talkback; do
            echo "$_s: $(pids_of "$(proc_name $_s)")"
        done
        echo "supervisor: pid $(cat "$SINGLE" 2>/dev/null) heartbeat $(beat_age)s ago"
        exit 0
        ;;
esac

# ---- singleton: replace a stalled supervisor, otherwise exit ----
_prev=$(cat "$SINGLE" 2>/dev/null)
case "$_prev" in ''|*[!0-9]*) _prev="" ;; esac
if [ -n "$_prev" ] && [ "$_prev" != "$$" ] && watchdog_pid "$_prev"; then
    _age=$(beat_age)
    if [ "$_age" -le "$STALE" ]; then
        echo "watchdog already running (pid $_prev, heartbeat ${_age}s)"
        exit 0
    fi
    echo "watchdog: pid $_prev stalled (no heartbeat for ${_age}s); replacing it"
    kill "$_prev" 2>/dev/null
    _i=0
    while [ -d "/proc/$_prev" ] && [ "$_i" -lt 25 ]; do sleep 0.2; _i=$((_i+1)); done
    [ -d "/proc/$_prev" ] && kill -9 "$_prev" 2>/dev/null
    # A stalled pass may have died holding the lock; only drop its own file.
    [ "$(cat "$LOCKF" 2>/dev/null)" = "$_prev" ] && rm -f "$LOCKF"
    rm -f "$SINGLE"
fi
# Is the pidfile owned by a live supervisor with a fresh heartbeat?
owner_fresh() {
    _o=$(cat "$SINGLE" 2>/dev/null)
    case "$_o" in ''|*[!0-9]*) return 1 ;; esac
    [ "$_o" != "$$" ] && watchdog_pid "$_o" && [ "$(beat_age)" -le "$STALE" ]
}
if ! ( set -C; echo $$ > "$SINGLE" ) 2>/dev/null; then
    if owner_fresh; then
        echo "watchdog already running (pid $(cat "$SINGLE"))"
        exit 0
    fi
    # A simultaneous starter may have claimed the file and not written its
    # first heartbeat yet; give it that window before overriding it.
    sleep 0.3
    if owner_fresh; then
        echo "watchdog already running (pid $(cat "$SINGLE"))"
        exit 0
    fi
    echo $$ > "$SINGLE" 2>/dev/null     # stale (owner gone / pid reused)
fi
beat
trap '[ "$(cat "$SINGLE" 2>/dev/null)" = "$$" ] && rm -f "$SINGLE"; lock_drop' EXIT
trap 'exit 143' INT TERM   # a trap alone would run its handler and keep looping

RMM_FAILS=0
MEDIAD_RESTARTS=0
MEDIAD_ALIVE_SINCE=0
while true; do
    beat
    sleep "$INTERVAL"
    beat
    trim_logs
    lock_take || { log "lock busy, skipping pass"; continue; }

    # Encoder: the boot-resolved choice ($ENCODER). Match the daemon binary path,
    # not "mediad" (mediad.sh's launcher line also contains it).
    if [ "$ENCODER" = "mediad" ]; then
        if alive "$YIP_PREFIX/bin/mediad"; then
            [ "$RMM_FAILS" -ne 0 ] && log "mediad present again (was $RMM_FAILS fails)"
            RMM_FAILS=0
            _now=$(now_up)
            case "$MEDIAD_ALIVE_SINCE" in ''|*[!0-9]*) MEDIAD_ALIVE_SINCE=$_now ;; esac
            [ "$MEDIAD_ALIVE_SINCE" -eq 0 ] && MEDIAD_ALIVE_SINCE=$_now
            # Only a run that lasted 10 min clears the ladder: mediad exits by
            # itself after 15 s without VI frames, so "alive at this pass" is
            # not proof of health and used to reset the count every time.
            if [ "$MEDIAD_RESTARTS" -ne 0 ] && \
               [ $((_now - MEDIAD_ALIVE_SINCE)) -ge 600 ]; then
                log "mediad up $((_now - MEDIAD_ALIVE_SINCE))s; clearing restart count (was $MEDIAD_RESTARTS)"
                MEDIAD_RESTARTS=0
            fi
        else
            MEDIAD_ALIVE_SINCE=0
            RMM_FAILS=$((RMM_FAILS+1))
            log "mediad MISSING ($RMM_FAILS/5)"
            if [ "$RMM_FAILS" -ge 5 ]; then
                MEDIAD_RESTARTS=$((MEDIAD_RESTARTS+1))
                if [ "$MEDIAD_RESTARTS" -le 3 ]; then
                    log "restarting mediad (attempt $MEDIAD_RESTARTS/3)"
                    "$YIP_PREFIX/script/mediad.sh" restart
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
