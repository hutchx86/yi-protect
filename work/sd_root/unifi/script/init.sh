#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors

# UniFi Protect emulation -- app init, launched by lower_half_init.sh.
# Starts the stock encoder (rmm) plus this project's stack (unifi_flv_bridge,
# unifi_avclient_go, talkback_rx, watchdog), reading only unifi.cfg.

UNIFI_PREFIX="/tmp/sd/unifi"
CONF="$UNIFI_PREFIX/etc/unifi.cfg"
MODEL_SUFFIX=$(cat "$UNIFI_PREFIX/etc/model_suffix" 2>/dev/null || echo y623)

# Emergency flash backup (idempotent; skips once a complete set exists).
if [ -x "$UNIFI_PREFIX/script/backup-flash.sh" ]; then
    "$UNIFI_PREFIX/script/backup-flash.sh" >>/tmp/sd/unifi-backup.log 2>&1 &
fi

get_cfg() {
    # Anchor to a real KEY= line so commented examples (#KEY=...) are not parsed.
    grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-
}

RESOLUTION=$(get_cfg RESOLUTION); [ -z "$RESOLUTION" ] && RESOLUTION=both
AUDIO=$(get_cfg AUDIO); [ -z "$AUDIO" ] && AUDIO=aac
# PTZ default by model (unifi.cfg PTZ= overrides); mirrors yi-hack's is_ptz().
PTZ=$(get_cfg PTZ)
if [ -z "$PTZ" ]; then
    case "$MODEL_SUFFIX" in
        r30gb|r35gb|r37gb|r40ga|q321br_lsx|qg311r|b091qp|h30ga|h51ga|h52ga|h60ga)
            PTZ=yes ;;
        *) PTZ=no ;;
    esac
fi
WATCHDOG_INTERVAL=$(get_cfg WATCHDOG_INTERVAL); [ -z "$WATCHDOG_INTERVAL" ] && WATCHDOG_INTERVAL=10

export PATH=/usr/bin:/usr/sbin:/bin:/sbin:/home/base/tools:/home/app/localbin:/home/base:$UNIFI_PREFIX/bin
export LD_LIBRARY_PATH=/lib:/usr/lib:/home/lib:/home/qigan/lib:/home/app/locallib:/tmp/sd:$UNIFI_PREFIX/lib
ulimit -s 1024

# MTU (stock cameras want 1500 on both interfaces)
[ -d /sys/class/net/eth0 ]  && echo 1500 > /sys/class/net/eth0/mtu
[ -d /sys/class/net/wlan0 ] && echo 1500 > /sys/class/net/wlan0/mtu

# ---- Ethernet/WiFi route preference ----
# If eth0 has carrier and a DHCP lease it is the preferred default route;
# otherwise WiFi. wlan0 is never taken down (the stock WiFi watchdog would just
# re-up it), so preference is enforced with route metrics: eth0 at 0, wlan0 at
# 100 -- WiFi stays associated as fallback. The stock app only runs DHCP on
# wlan0, so we run our own eth0 client. default.script stores each gateway in
# /tmp/gw0 (eth0) and /tmp/gw1 (wlan0); we re-assert the metrics when they
# drift. avclient is restarted on a real state change so it re-detects its
# identity. Logs to /tmp/network.log.
eth_dhcp() {
    DS=/backup/tools/default.script
    [ -f "$DS" ] || DS=/home/app/script/default.script
    [ -f "$DS" ] || return 0
    udhcpc -i eth0 -b -O 43 -V ubnt -s "$DS" -x hostname:unifi >/dev/null 2>&1 &
}

# Default-route metric for $1 via busybox `route -n` ($5=metric, $8=iface).
iface_metric() {
    route -n 2>/dev/null | awk -v d="$1" '$1=="0.0.0.0" && $8==d {print $5; exit}'
}

# Ensure $1's default route via $2 sits at metric $3; no-op if unchanged.
set_default_metric() {
    dev="$1"; gw="$2"; want="$3"
    [ -z "$gw" ] && return 0
    [ "$(iface_metric "$dev")" = "$want" ] && return 0
    route del default dev "$dev" 2>/dev/null
    route add default gw "$gw" dev "$dev" metric "$want" 2>/dev/null
}

net_monitor() {
    state=""
    while true; do
        carrier=0
        [ -d /sys/class/net/eth0 ] && carrier=$(cat /sys/class/net/eth0/carrier 2>/dev/null || echo 0)

        if [ "$carrier" = "1" ]; then
            ifconfig eth0 up 2>/dev/null
            if ! ifconfig eth0 2>/dev/null | grep -q "inet addr"; then
                eth_dhcp
            fi
        fi

        if [ "$carrier" = "1" ] && ifconfig eth0 2>/dev/null | grep -q "inet addr"; then
            if [ "$state" != "eth" ]; then
                echo "$(date +%H:%M:%S) eth0 link+lease -> Ethernet preferred (wlan0 stays up, metric 100)" >> /tmp/network.log
                state=eth
                killall unifi_avclient_go 2>/dev/null
            fi
            set_default_metric eth0 "$(cat /tmp/gw0 2>/dev/null)" 0
            set_default_metric wlan0 "$(cat /tmp/gw1 2>/dev/null)" 100
        else
            # No usable Ethernet -> WiFi is the default.
            if [ "$state" != "wifi" ]; then
                echo "$(date +%H:%M:%S) eth0 no link/lease -> WiFi" >> /tmp/network.log
                if [ -d /sys/class/net/wlan0 ]; then
                    ifconfig wlan0 up 2>/dev/null
                    if ! ifconfig wlan0 2>/dev/null | grep -q "inet addr"; then
                        DS=/backup/tools/default.script
                        [ -f "$DS" ] || DS=/home/app/script/default.script
                        [ -f "$DS" ] && udhcpc -i wlan0 -b -O 43 -V ubnt -s "$DS" -x hostname:unifi >/dev/null 2>&1 &
                    fi
                fi
                # Cable out: drop eth0's default and stale address, keep PHY up
                # so carrier stays readable.
                route del default dev eth0 2>/dev/null
                if ifconfig eth0 2>/dev/null | grep -q "inet addr"; then
                    ifconfig eth0 down 2>/dev/null
                fi
                ifconfig eth0 up 2>/dev/null
                state=wifi
                killall unifi_avclient_go 2>/dev/null
            fi
            # Re-assert WiFi as metric 0 in case a prior Ethernet pass raised it.
            set_default_metric wlan0 "$(cat /tmp/gw1 2>/dev/null)" 0
        fi
        sleep 5
    done
}

if [ -d /sys/class/net/eth0 ]; then
    net_monitor &
fi

# Make /etc writable (some stock bits write there)
mkdir -p /tmp/etc && cp -R /etc/* /tmp/etc 2>/dev/null && mount --bind /tmp/etc /etc

# ---- swap ----
# Stock yi-hack system.sh created /tmp/sd/swapfile and enabled it; our init
# replaced system.sh and dropped that, leaving zero swap on a 60MB board. A
# snapshot (imggrabber/ffmpeg, ~9MB) could then OOM-kill rmm, and the stock
# watch_process reboots when rmm dies. Restore the swapfile.
if mount 2>/dev/null | grep -q "/tmp/sd "; then
    SWAPFILE=/tmp/sd/swapfile
    if [ ! -f "$SWAPFILE" ]; then
        dd if=/dev/zero of="$SWAPFILE" bs=1M count=64 2>/dev/null
    fi
    chmod 0600 "$SWAPFILE"
    # No mkswap on this box (no util-linux; busybox applet not compiled in),
    # so write the version-1 header by hand. Page size 4096, file 64 MiB ->
    # last_page = 16383.
    #   offset 1024: version=1; 1028: last_page=16383; 1032: nr_badpages=0
    #   offset 4086: magic "SWAPSPACE2"
    printf '\001\000\000\000\377\077\000\000\000\000\000\000' \
        | dd of="$SWAPFILE" bs=1 seek=1024 conv=notrunc 2>/dev/null
    printf 'SWAPSPACE2' \
        | dd of="$SWAPFILE" bs=1 seek=4086 conv=notrunc 2>/dev/null
    swapon "$SWAPFILE" 2>/dev/null
    # Low swappiness: plenty for OOM headroom, gentle on SD-card wear.
    echo 15 > /proc/sys/vm/swappiness 2>/dev/null
fi

# ---- SSH (dropbear), started early ----
# Kept before the video pipeline so a failure there can't lock us out. Generate
# both ECDSA and ED25519 keys up front (stock clients prefer ed25519; missing
# keys made dropbear's on-demand generation fail the handshake) and keep the
# first login fast.
DBDIR="$UNIFI_PREFIX/etc/dropbear"
mkdir -p "$DBDIR"
for kt in ecdsa ed25519; do
    [ -f "$DBDIR/dropbear_${kt}_host_key" ] || \
        dropbearmulti dropbearkey -t "$kt" -f "$DBDIR/dropbear_${kt}_host_key" >/dev/null 2>&1
done
DBKEYS=""
for kt in ecdsa ed25519; do
    [ -f "$DBDIR/dropbear_${kt}_host_key" ] && DBKEYS="$DBKEYS -r $DBDIR/dropbear_${kt}_host_key"
done
dropbearmulti dropbear -R $DBKEYS -B -p 0.0.0.0:22

# Hide the stock Yi watermark: bind all-white blanks (the OSD's transparent
# colour key) of the same size over every stock watermark bitmap.
[ -f "$UNIFI_PREFIX/etc/main_blank.bmp" ] && mount --bind "$UNIFI_PREFIX/etc/main_blank.bmp" /home/app/main.bmp
[ -f "$UNIFI_PREFIX/etc/main_blank.bmp" ] && mount --bind "$UNIFI_PREFIX/etc/main_blank.bmp" /home/app/main_kami.bmp
[ -f "$UNIFI_PREFIX/etc/sub_blank.bmp" ]  && mount --bind "$UNIFI_PREFIX/etc/sub_blank.bmp"  /home/app/sub.bmp
[ -f "$UNIFI_PREFIX/etc/sub_blank.bmp" ]  && mount --bind "$UNIFI_PREFIX/etc/sub_blank.bmp"  /home/app/sub_kami.bmp

# Controller override is read by unifi_avclient_go from unifi.cfg; the
# unifi_client_go.inform-host* files are runtime state only.

# Speaker/talkback FIFO: rmm opens /tmp/audio_in_fifo for speaker playback. The
# stock firmware creates it from the .requested marker; our init must mknod it.
touch /tmp/audio_in_fifo.requested
[ -p /tmp/audio_in_fifo ] || mknod /tmp/audio_in_fifo p

# ---- stock media daemon (rmm) ----
# rmm publishes /dev/shm/fshare_frame_buf, which unifi_flv_bridge reads below.
cd /home/app
sleep 2
./rmm > /tmp/rmm.log 2>&1 &
RMM_PID=$!
# Keep rmm off the OOM killer's list (60MB box; snapshots are the victims).
echo -1000 > "/proc/$RMM_PID/oom_score_adj" 2>/dev/null
sleep 4

# Kick the encoder ring so rmm actually starts producing (else the bridge sees
# an empty ring forever). Same trick as the stock system.sh: a brief `cloud`
# run fills the circular buffer, then `ipc_cmd -x` starts the stream.
./cloud >/dev/null 2>&1 &
IDX=$(hexdump -n 16 /dev/shm/fshare_frame_buf | awk 'NR==1{print $8}')
N=0
while [ "$IDX" = "0000" ] && [ "$N" -lt 60 ]; do
    IDX=$(hexdump -n 16 /dev/shm/fshare_frame_buf | awk 'NR==1{print $8}')
    N=$((N+1))
    sleep 0.2
done
killall -q cloud
ipc_cmd -x

# Re-arm the stock AI/motion flags on 11.x/12.x firmware so a reboot doesn't
# lose persisted detection state (ipc_cmd is in our bin/).
HOMEVER=$(cat /home/homever)
HV=${HOMEVER:0:2}
if [ "${HV:1:1}" = "." ]; then HV=${HV:0:1}; fi
if [ "$HV" = "11" ] || [ "$HV" = "12" ]; then
    ipc_cmd -1
    sleep 0.5
    ipc_cmd -O on
fi

# ---- this project's stack ----
cd "$UNIFI_PREFIX/bin"

# Video push daemon (reads rmm's shared-memory stream, pushes extendedFlv).
AUDIO_OPT="-a $AUDIO"
./unifi_flv_bridge -m "$MODEL_SUFFIX" -r "$RESOLUTION" -s $AUDIO_OPT > /tmp/unifi_flv_bridge.log 2>&1 &

# Adoption/control client; cert/key are self-generated on first boot. -ptz
# declares the "ptz" featureFlag so Protect shows PTZ controls.
PTZ_OPT=""
[ "$PTZ" = "yes" ] && PTZ_OPT="-ptz"
./unifi_avclient_go \
    $PTZ_OPT \
    -cert "$UNIFI_PREFIX/etc/unifi_client_go.crt" \
    -key  "$UNIFI_PREFIX/etc/unifi_client_go.key" \
    > /tmp/avclient.log 2>&1 &

# Talkback (speaker) receiver.
./talkback_rx > /tmp/talkback_rx.log 2>&1 &

# Watchdog.
WATCHDOG_INTERVAL=$WATCHDOG_INTERVAL "$UNIFI_PREFIX/script/watchdog.sh" > /tmp/watchdog.log 2>&1 &

exit 0
