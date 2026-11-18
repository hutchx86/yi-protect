#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# wifi-guard -- keep WiFi provisioned across a stock factory reset.
#
# The stock reset button (dispatch -> set_default()) zeroes the conf partition,
# /dev/mtdblock7, which is where the stock boot path reads the SSID and PSK.
# The SD payload survives, so on the next boot this guard:
#   - restores the last known-good config and reboots once when the SSID is empty
#   - clears the Protect adoption identity, so the camera comes back unadopted
#   - applies a pending configure_wifi.cfg on any boot (the normal change path)
# A failed mtd7 read is not a wipe: it must never clear adoption.
#
# Exit 0 on a healthy boot or after an action; the caller continues normally.

YIP="${WIFI_GUARD_YIP:-/tmp/sd/yi-protect}"
MTD="${WIFI_GUARD_MTD:-/dev/mtdblock7}"
REBOOT_CMD="${WIFI_GUARD_REBOOT:-reboot}"
DO_KILL="${WIFI_GUARD_KILL:-1}"
LOG="${WIFI_GUARD_LOG:-/tmp/wifi-guard.log}"

ETC="$YIP/etc"
WCFG="$ETC/configure_wifi.cfg"
KNOWN="$ETC/wifi.known-good"
APPLIED="$WCFG.applied"

log() {
    _line="$(date +%H:%M:%S 2>/dev/null) $*"
    echo "$_line" >> "$LOG" 2>/dev/null
    echo "wifi-guard: $*"
}

# Report "present", "empty" or "error" for the 64-byte SSID field at offset 28.
# The empty field is all zero bytes; any non-zero nibble means provisioned.
# Uses the default hexdump output (this busybox has no hexdump -e); drop the
# offset column, then look for a non-zero hex digit.
ssid_state() {
    _out=$(hexdump -s 28 -n 64 -v "$MTD" 2>/dev/null)
    if [ -z "$_out" ]; then
        echo error
        return 0
    fi
    _hex=$(printf '%s\n' "$_out" | awk '{$1=""; print}' | tr -d ' \n')
    case "$_hex" in
        '') echo error ;;
        *[1-9a-f]*) echo present ;;
        *) echo empty ;;
    esac
}

# The stock app owns the reset path; stop it before we rewrite the partition it
# may have just zeroed. Leave the supplicant alone: if the restore fails, the
# existing association keeps the camera reachable until someone fixes the card.
stop_stock_wifi() {
    [ "$DO_KILL" = "1" ] || return 0
    killall dispatch 2>/dev/null
    sleep 1
}

clear_adoption() {
    [ -d "$ETC" ] || { log "no $ETC; cannot clear adoption"; return 1; }
    for _f in yi_protect_client_go.crt yi_protect_client_go.key \
              yi_protect_client_go.device-id yi_protect_client_go.adoption-uuid; do
        rm -f "$ETC/$_f" 2>/dev/null
    done
    printf 'false\n' > "$ETC/yi_protect_client_go.adopted" 2>/dev/null \
        || log "could not persist adopted=false"
    printf 'stock-wifi-reset\n' > "$ETC/yi_protect_client_go.awaiting-manage" 2>/dev/null \
        || log "could not persist awaiting-manage"
    log "adoption identity cleared; camera will wait for a controller adopt push"
}

save_known() {
    [ -f "$WCFG" ] || return 0
    cp "$WCFG" "$KNOWN" 2>/dev/null || log "could not save $KNOWN"
}

# Apply $1, which may be the pending cfg itself. configure-wifi.sh exits 0 when
# it wrote mtd7, 2 when the partition already matches, 1 on an error.
apply_cfg() {
    _src="$1"
    if [ "$_src" != "$WCFG" ]; then
        cp "$_src" "$WCFG" 2>/dev/null || { log "could not restore $_src"; return 1; }
    fi
    MTD="$MTD" sh "$YIP/script/configure-wifi.sh" "$WCFG"
}

do_reboot() {
    save_known
    mv "$WCFG" "$APPLIED" 2>/dev/null
    sync 2>/dev/null
    log "rebooting to associate"
    $REBOOT_CMD
}

# A pending cfg is the normal change path, even on an already-provisioned unit.
# It is not a factory reset, so adoption is left alone.
if [ -f "$WCFG" ]; then
    apply_cfg "$WCFG"
    rc=$?
    case $rc in
        0)
            if [ "$(ssid_state)" = "present" ]; then
                do_reboot
            else
                log "pending $WCFG did not take (mtd7 still empty); leaving it in place"
            fi
            ;;
        2) save_known; mv "$WCFG" "$APPLIED" 2>/dev/null; sync 2>/dev/null ;;
        *) log "pending $WCFG failed (rc=$rc); leaving it in place" ;;
    esac
    exit 0
fi

state=$(ssid_state)
case "$state" in
    present) exit 0 ;;
    error)
        log "cannot read the SSID from $MTD; leaving adoption and WiFi alone"
        exit 0
        ;;
esac

# The SSID is empty: a stock reset (or a never-provisioned card). Prefer the
# last successful config; .applied covers cards written before this guard.
SRC=""
if [ -f "$KNOWN" ]; then
    SRC="$KNOWN"
elif [ -f "$APPLIED" ]; then
    SRC="$APPLIED"
fi

if [ -z "$SRC" ]; then
    log "WiFi is unprovisioned and no known-good config is on the SD; leaving adoption alone"
    exit 0
fi

stop_stock_wifi

# The stock app may have restored the partition between the read and the kill.
state=$(ssid_state)
if [ "$state" = "present" ]; then
    log "WiFi reappeared before restore; leaving adoption alone"
    exit 0
fi

apply_cfg "$SRC"
rc=$?
case $rc in
    0)
        # configure-wifi.sh ignores dd write errors, so verify the field took
        # before clearing adoption: a failed restore must leave the camera
        # recoverable, not unadopted with no network.
        if [ "$(ssid_state)" = "present" ]; then
            clear_adoption
            do_reboot
        else
            log "restore from $SRC did not take (mtd7 still empty); leaving adoption alone"
        fi
        ;;
    2)
        # The partition already matches; only dispatch was stopped, so reboot
        # to bring the stock app back without touching adoption.
        log "WiFi was already provisioned before restore; restarting the stock app"
        do_reboot
        ;;
    *)
        log "restore from $SRC failed (rc=$rc); leaving adoption alone"
        ;;
esac
exit 0
