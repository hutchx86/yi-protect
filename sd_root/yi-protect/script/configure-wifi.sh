#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# configure-wifi -- write WiFi station credentials into the conf partition
# (/dev/mtdblock7), standing in for the Yi app's pairing step; no cloud needed.
#
# Layout: off 24 4B flag (0 = associated), off 28 64B SSID, off 92 64B PSK (NUL-padded).
#
# Usage: configure-wifi.sh [cfgfile] (KEY=value wifi_ssid=/wifi_psk=; the value
# may be bare or wrapped in one pair of matching single/double quotes).
# Exit: 0=written (reboot), 2=already current, 1=error.

MTD="${MTD:-/dev/mtdblock7}"
CFG="${1:-/tmp/sd/yi-protect/etc/configure_wifi.cfg}"

die() { echo "configure-wifi: $*" >&2; exit 1; }

# Strip one pair of matching surrounding quotes, so both wifi_ssid=My Net and
# wifi_ssid="My Net" (or 'My Net') are accepted. Bare values pass through.
unquote() {
    local v="$1"
    case $v in
        \"*\") v=${v#\"}; v=${v%\"} ;;
        \'*\') v=${v#\'}; v=${v%\'} ;;
    esac
    printf '%s' "$v"
}

[ -e "$MTD" ] || die "$MTD not present"
[ -f "$CFG" ] || die "config not found: $CFG"

SSID=$(unquote "$(grep -E '^wifi_ssid=' "$CFG" 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '\r')")
PSK=$(unquote "$(grep -E '^wifi_psk='  "$CFG" 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '\r')")

[ -n "$SSID" ] || die "wifi_ssid is empty in $CFG"
[ -n "$PSK" ]  || die "wifi_psk is empty in $CFG"
[ "${#SSID}" -le 63 ] || die "ssid too long (${#SSID} > 63)"
[ "${#PSK}"  -le 63 ] || die "psk too long (${#PSK} > 63)"

# Current values (trailing NUL padding is dropped by command substitution, so a
# plain string compare works). hexdump flag read mirrors yi-hack's configure_wifi.sh.
CUR_SSID=$(dd bs=1 skip=28 count=64 if="$MTD" 2>/dev/null)
CUR_PSK=$(dd bs=1 skip=92 count=64 if="$MTD" 2>/dev/null)
CUR_FLAG=$(hexdump -s 24 -n 4 -v "$MTD" 2>/dev/null | awk 'NR==1{print $3$2}')

if [ "$SSID" = "$CUR_SSID" ] && [ "$PSK" = "$CUR_PSK" ] && [ "$CUR_FLAG" = "00000000" ]; then
    echo "configure-wifi: already provisioned for '$SSID'"
    exit 2
fi

# Back up the conf partition before touching it (project rule: keep flash
# artifacts). /tmp/sd is the FAT card root; skip if not mounted.
if [ -d /tmp/sd ]; then
    BAK="/tmp/sd/mtdblock7_$(date '+%Y%m%d%H%M%S').bin"
    dd if="$MTD" of="$BAK" 2>/dev/null && echo "configure-wifi: backup -> $BAK"
fi

# Zero the fields first so a shorter value can't leave the tail of an older one.
dd if=/dev/zero of="$MTD" bs=1 seek=28 count=64 conv=notrunc 2>/dev/null
dd if=/dev/zero of="$MTD" bs=1 seek=92 count=64 conv=notrunc 2>/dev/null
printf '%s' "$SSID" | dd of="$MTD" bs=1 seek=28 count=64 conv=notrunc 2>/dev/null
printf '%s' "$PSK"  | dd of="$MTD" bs=1 seek=92 count=64 conv=notrunc 2>/dev/null
printf '\0\0\0\0' | dd of="$MTD" bs=1 seek=24 count=4 conv=notrunc 2>/dev/null

sync; sync; sync
echo "configure-wifi: wrote '$SSID' to $MTD (reboot to associate)"
exit 0
