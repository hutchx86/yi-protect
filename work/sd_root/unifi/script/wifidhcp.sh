#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Vendored from yi-hack's wifidhcp.sh (GPLv3), repathed to /tmp/sd/unifi/etc.
# The stock firmware bind-mounts this over /home/app/script and /backup/tools.
UNIFI_PREFIX=/tmp/sd/unifi
CONF_FILE="etc/unifi.cfg"
HN=$UNIFI_PREFIX/etc/hostname

killall udhcpc

get_config() {
    grep -E "^$1=" "$UNIFI_PREFIX/$CONF_FILE" 2>/dev/null | cut -d= -f2-
}

STATIC_IP=$(get_config STATIC_IP)
STATIC_MASK=$(get_config STATIC_MASK)
STATIC_GW=$(get_config STATIC_GW)
STATIC_DNS1=$(get_config STATIC_DNS1)
STATIC_DNS2=$(get_config STATIC_DNS2)
[ -f "$HN" ] || HN="unifi"

if [ -z "$STATIC_IP" ] || [ -z "$STATIC_MASK" ]; then
    if [ -f /backup/tools/default.script ]; then
        udhcpc -i wlan0 -b -O 43 -V ubnt -s /backup/tools/default.script -x hostname:$(cat $HN)
    elif [ -f /home/app/script/default.script ]; then
        udhcpc -i wlan0 -b -O 43 -V ubnt -s /home/app/script/default.script -x hostname:$(cat $HN)
    fi
else
    ifconfig wlan0 $STATIC_IP netmask $STATIC_MASK
    [ -n "$STATIC_GW" ] && route add -net 0.0.0.0 gw $STATIC_GW
    if [ -n "$STATIC_DNS1" ] || [ -n "$STATIC_DNS2" ]; then
        rm -f /tmp/resolv.conf
        touch /tmp/resolv.conf
        [ -n "$STATIC_DNS1" ] && echo "nameserver $STATIC_DNS1" >> /tmp/resolv.conf
        [ -n "$STATIC_DNS2" ] && echo "nameserver $STATIC_DNS2" >> /tmp/resolv.conf
    fi
fi
