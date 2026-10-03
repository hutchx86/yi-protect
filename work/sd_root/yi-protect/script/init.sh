#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors

# UniFi Protect emulation -- app init, launched by lower_half_init.sh. Starts the
# encoder (mediad if installed, else stock rmm) plus our stack, reading yi-protect.cfg.

YIP_PREFIX="/tmp/sd/yi-protect"
CONF="$YIP_PREFIX/etc/yi-protect.cfg"
MODEL_SUFFIX=$(cat "$YIP_PREFIX/etc/model_suffix" 2>/dev/null || echo y623)

# Emergency flash backup (idempotent; skips once a complete set exists).
if [ -x "$YIP_PREFIX/script/backup-flash.sh" ]; then
    "$YIP_PREFIX/script/backup-flash.sh" >>/tmp/sd/yi-protect-backup.log 2>&1 &
fi

get_cfg() {
    # Anchor to a real KEY= line so commented examples (#KEY=...) are not parsed.
    grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-
}

RESOLUTION=$(get_cfg RESOLUTION); [ -z "$RESOLUTION" ] && RESOLUTION=both
AUDIO=$(get_cfg AUDIO); [ -z "$AUDIO" ] && AUDIO=aac
WATCHDOG_INTERVAL=$(get_cfg WATCHDOG_INTERVAL); [ -z "$WATCHDOG_INTERVAL" ] && WATCHDOG_INTERVAL=10
YI_CLOUD=$(get_cfg YI_CLOUD)
if [ -z "$YI_CLOUD" ]; then
    # Default on, except on mediad builds (IS_MEDIAD=yes): mediad replaces the
    # stock encoder daemon and the Yi cloud is not run alongside it.
    if [ "$(get_cfg IS_MEDIAD)" = "yes" ]; then YI_CLOUD=no; else YI_CLOUD=yes; fi
fi

export PATH=/usr/bin:/usr/sbin:/bin:/sbin:/home/base/tools:/home/app/localbin:/home/base:$YIP_PREFIX/bin
export LD_LIBRARY_PATH=/lib:/usr/lib:/home/lib:/home/qigan/lib:/home/app/locallib:/tmp/sd:$YIP_PREFIX/lib
ulimit -s 1024

# MTU (stock cameras want 1500 on both interfaces)
[ -d /sys/class/net/eth0 ]  && echo 1500 > /sys/class/net/eth0/mtu
[ -d /sys/class/net/wlan0 ] && echo 1500 > /sys/class/net/wlan0/mtu

# Route preference: eth0 (if carrier+lease) at metric 0, else wlan0 at 100;
# wlan0 is never taken down. Gateways: /tmp/gw0 (eth0), /tmp/gw1 (wlan0).
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
                killall yi_protect_avclient_go 2>/dev/null
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
                killall yi_protect_avclient_go 2>/dev/null
            fi
            # Re-assert WiFi as metric 0 in case a prior Ethernet pass raised it.
            set_default_metric wlan0 "$(cat /tmp/gw1 2>/dev/null)" 0
            # Disable 802.11 power-save (driver default on buffers unicast ->
            # live-view stutter). Re-applied each pass: reassociation resets it.
            iwconfig wlan0 power off 2>/dev/null
        fi
        sleep 5
    done
}

if [ -d /sys/class/net/eth0 ]; then
    net_monitor &
fi

# Make /etc writable (some stock bits write there)
mkdir -p /tmp/etc && cp -R /etc/* /tmp/etc 2>/dev/null && mount --bind /tmp/etc /etc

# Swap: stock system.sh made /tmp/sd/swapfile; we replaced system.sh and dropped
# it, leaving a 60MB board swapless (snapshot OOM could kill rmm). Restore it.
if mount 2>/dev/null | grep -q "/tmp/sd "; then
    SWAPFILE=/tmp/sd/swapfile
    if [ ! -f "$SWAPFILE" ]; then
        dd if=/dev/zero of="$SWAPFILE" bs=1M count=64 2>/dev/null
    fi
    chmod 0600 "$SWAPFILE"
    # No mkswap on this box, so write the version-1 header by hand. Page 4096, file
    # 64 MiB: @1024 version=1, @1028 last_page=16383, @1032 nr_badpages=0, @4086 magic SWAPSPACE2.
    printf '\001\000\000\000\377\077\000\000\000\000\000\000' \
        | dd of="$SWAPFILE" bs=1 seek=1024 conv=notrunc 2>/dev/null
    printf 'SWAPSPACE2' \
        | dd of="$SWAPFILE" bs=1 seek=4086 conv=notrunc 2>/dev/null
    swapon "$SWAPFILE" 2>/dev/null
    # Low swappiness: plenty for OOM headroom, gentle on SD-card wear.
    echo 15 > /proc/sys/vm/swappiness 2>/dev/null
fi

# WiFi provisioning (SD config): drop yi-protect/etc/configure_wifi.cfg (wifi_ssid=/
# wifi_psk=) and reboot; init.sh writes mtd7 and reboots once, renaming to .applied.
WCFG="$YIP_PREFIX/etc/configure_wifi.cfg"
if [ -f "$WCFG" ] && [ -x "$YIP_PREFIX/script/configure-wifi.sh" ]; then
    "$YIP_PREFIX/script/configure-wifi.sh" "$WCFG"
    rc=$?
    case $rc in
        0) mv "$WCFG" "$WCFG.applied"; sync; echo "init: wifi credentials applied; rebooting"; reboot ;;
        2) mv "$WCFG" "$WCFG.applied"; sync ;;
        *) echo "init: wifi provisioning failed (rc=$rc); leaving $WCFG in place" ;;
    esac
fi

# SSH (dropbear) started before the video pipeline so a failure there can't lock
# us out. Generate ecdsa+ed25519 host keys up front (missing keys failed the KEX).
DBDIR="$YIP_PREFIX/etc/dropbear"
mkdir -p "$DBDIR"
for kt in ecdsa ed25519; do
    [ -f "$DBDIR/dropbear_${kt}_host_key" ] || \
        dropbearmulti dropbearkey -t "$kt" -f "$DBDIR/dropbear_${kt}_host_key" >/dev/null 2>&1
done
DBKEYS=""
for kt in ecdsa ed25519; do
    [ -f "$DBDIR/dropbear_${kt}_host_key" ] && DBKEYS="$DBKEYS -r $DBDIR/dropbear_${kt}_host_key"
done

# Accounts: ssh-accounts.sh. No -B: stock root has a blank password, and an
# empty SSH_PASSWORD means "locked", never "open".
YIP_PREFIX="$YIP_PREFIX" sh "$YIP_PREFIX/script/ssh-accounts.sh"
dropbearmulti dropbear -R $DBKEYS -p 0.0.0.0:22

# Hide the stock Yi watermark: bind all-white blanks (the OSD's transparent
# colour key) of the same size over every stock watermark bitmap.
[ -f "$YIP_PREFIX/etc/main_blank.bmp" ] && mount --bind "$YIP_PREFIX/etc/main_blank.bmp" /home/app/main.bmp
[ -f "$YIP_PREFIX/etc/main_blank.bmp" ] && mount --bind "$YIP_PREFIX/etc/main_blank.bmp" /home/app/main_kami.bmp
[ -f "$YIP_PREFIX/etc/sub_blank.bmp" ]  && mount --bind "$YIP_PREFIX/etc/sub_blank.bmp"  /home/app/sub.bmp
[ -f "$YIP_PREFIX/etc/sub_blank.bmp" ]  && mount --bind "$YIP_PREFIX/etc/sub_blank.bmp"  /home/app/sub_kami.bmp

# Controller override is read by yi_protect_avclient_go from yi-protect.cfg; the
# yi_protect_client_go.inform-host* files are runtime state only.

# Speaker/talkback FIFO: rmm opens /tmp/audio_in_fifo for speaker playback. The
# stock firmware creates it from the .requested marker; our init must mknod it.
touch /tmp/audio_in_fifo.requested
[ -p /tmp/audio_in_fifo ] || mknod /tmp/audio_in_fifo p

# Yi cloud daemons (YI_CLOUD=yes): cloud/p2p_tnp/oss in /home/app. In the rmm
# path cloud already runs as the ring kicker, so start it only when absent.
start_yi_cloud() {
    cd /home/app
    if ! ps | grep -v grep | grep -q '[c]loud'; then
        ./cloud >/dev/null 2>&1 &
    fi
    ./p2p_tnp  >/dev/null 2>&1 &
    ./oss      >/dev/null 2>&1 &
    [ -f ./oss_fast ]  && ./oss_fast  >/dev/null 2>&1 &
    [ -f ./oss_lapse ] && ./oss_lapse >/dev/null 2>&1 &
}

# The vendor H.264 libs (libvenc_codec.so, libVE.so) are needed only by a
# vendor-linked mediad build; the default build links its own encoder.
if grep -q 'libvenc_codec.so' "$YIP_PREFIX/bin/mediad" 2>/dev/null; then
    # From the pinned lindenis SDK; no CA store on the camera, so -k (md5-checked).
    YIP_DIR=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)
    [ -n "$YIP_DIR" ] && [ -d "$YIP_DIR/script" ] || YIP_DIR="$YIP_PREFIX"
    FETCH_BIN="$YIP_DIR/bin/downloader"
    LIBFETCH_LOG=/tmp/libfetch.log
    SDK_URL="https://raw.githubusercontent.com/lindenis-org/lindenis-v833-softwinner/834a5afe83ec037a38ed0dbed522ff65439eabdf/eyesee-mpp/middleware/sun8iw19p1/media/LIBRARY/libcedarc/library"

    # Poll up to ~45s for network/DNS before fetching (cold-boot DHCP/DNS is often
    # late): ping proves DNS+ICMP, a tiny probe download proves downloader/TLS.
    PROBE_URL="$SDK_URL/tina.mk"
    net_ready() {
        ping -c 1 -W 2 github.com >/dev/null 2>&1 && return 0
        [ -x "$FETCH_BIN" ] || return 1
        "$FETCH_BIN" -k "$PROBE_URL" /tmp/.netprobe >/dev/null 2>&1 \
            && { rm -f /tmp/.netprobe; return 0; }
        return 1
    }
    i=0
    while [ "$i" -lt 15 ]; do
        if net_ready; then
            echo "libfetch: network/DNS ready after $((i*3))s" >> "$LIBFETCH_LOG"
            break
        fi
        i=$((i+1))
        echo "libfetch: waiting for network/DNS (${i}/15)" >> "$LIBFETCH_LOG"
        sleep 3
    done
    [ "$i" -ge 15 ] && echo "libfetch: network/DNS not ready after 45s; proceeding anyway" >> "$LIBFETCH_LOG"

    # ensure_lib: skip if md5 matches, else download to .tmp and verify. WiFi resets
    # mid-body, so the downloader resumes .tmp via Range; keep it across the 12 tries.
    ensure_lib() {
        dst="$YIP_DIR/lib/$1"
        [ "$(md5sum "$dst" 2>/dev/null | awk '{print $1}')" = "$2" ] && return 0
        if [ ! -x "$FETCH_BIN" ]; then
            echo "libfetch: $FETCH_BIN missing; cannot fetch $1" >> "$LIBFETCH_LOG"
            return 1
        fi
        tmp="$dst.tmp"
        rm -f "$tmp"
        n=0
        while [ "$n" -lt 12 ]; do
            n=$((n+1))
            if "$FETCH_BIN" -k "$SDK_URL/$1" "$tmp" >> "$LIBFETCH_LOG" 2>&1 \
               && [ "$(md5sum "$tmp" 2>/dev/null | awk '{print $1}')" = "$2" ]; then
                mv -f "$tmp" "$dst"
                echo "libfetch: $1 fetched (md5 ok)" >> "$LIBFETCH_LOG"
                return 0
            fi
            echo "libfetch: $1 fetch/verify failed (attempt $n)" >> "$LIBFETCH_LOG"
            sleep 2
        done
        rm -f "$tmp"
        return 1
    }

    ensure_lib libvenc_codec.so 8888f9a820021484e1cea01efd8142e6
    ensure_lib libVE.so          096259a6c6178dee25e9dda61b01b715
    for l in libvenc_codec.so libVE.so; do
        [ -f "$YIP_DIR/lib/$l" ] || echo "libfetch: WARNING: $l still missing; mediad will fail to load the H.264 encoder" >> "$LIBFETCH_LOG"
    done
fi

# Media daemon: mediad (sister project's rmm replacement) if installed, else
# stock rmm; both publish /dev/shm/fshare_frame_buf. Never run both at once.
IS_MEDIAD=$(get_cfg IS_MEDIAD); [ -z "$IS_MEDIAD" ] && IS_MEDIAD=no
if [ "$IS_MEDIAD" = "yes" ] && [ -x "$YIP_PREFIX/bin/mediad" ] && [ -x "$YIP_PREFIX/script/mediad.sh" ]; then
    # Tell the bridge/client which encoder is live so they declare the matching
    # model_table geometry (h51ga: mediad streams native 1080p, rmm upscales to 2K).
    export YIP_ENCODER=mediad
    # CABAC is the encoder default (cabac_init_idc=1); do not pin cabac=0.
    # overlay=1 enables the burned-in OSD overlay path.
    export FREECODEC_EXTRA="overlay=1"
    echo "init: starting mediad (FREECODEC_EXTRA=$FREECODEC_EXTRA)"
    "$YIP_PREFIX/script/mediad.sh" start
    sleep 2
    [ "$YI_CLOUD" = "yes" ] && start_yi_cloud
else
    export YIP_ENCODER=rmm
    cd /home/app
    sleep 2
    # Load the SD-shipped patched libasound (the only one with the /tmp/audio_in_fifo
    # listener talkback needs, else talkback fails ENXIO); prepend its lib dir.
    LD_LIBRARY_PATH="$YIP_PREFIX/lib:$LD_LIBRARY_PATH" ./rmm > /tmp/rmm.log 2>&1 &
    RMM_PID=$!
    # Keep rmm off the OOM killer's list (60MB box; snapshots are the victims).
    echo -1000 > "/proc/$RMM_PID/oom_score_adj" 2>/dev/null
    sleep 4

    # Kick the encoder ring so rmm starts producing: a brief `cloud` run fills the
    # buffer, then `ipc_cmd -x`. (mediad produces on its own.) YI_CLOUD keeps cloud.
    ./cloud >/dev/null 2>&1 &
    IDX=$(hexdump -n 16 /dev/shm/fshare_frame_buf | awk 'NR==1{print $8}')
    N=0
    while [ "$IDX" = "0000" ] && [ "$N" -lt 60 ]; do
        IDX=$(hexdump -n 16 /dev/shm/fshare_frame_buf | awk 'NR==1{print $8}')
        N=$((N+1))
        sleep 0.2
    done
    if [ "$YI_CLOUD" = "yes" ]; then
        start_yi_cloud
        echo "init: Yi cloud daemons running (cloud/p2p_tnp/oss)"
    else
        killall -q cloud
    fi
    ipc_cmd -x
fi

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
cd "$YIP_PREFIX/bin"

# Video push daemon (reads rmm's shared-memory stream, pushes extendedFlv).
AUDIO_OPT="-a $AUDIO"
./yi_protect_flv_bridge -m "$MODEL_SUFFIX" -r "$RESOLUTION" $AUDIO_OPT > /tmp/yi_protect_flv_bridge.log 2>&1 &

# Adoption/control client; cert/key self-generated on first boot. PTZ and
# per-model geometry come from model_table.
./yi_protect_avclient_go \
    -cert "$YIP_PREFIX/etc/yi_protect_client_go.crt" \
    -key  "$YIP_PREFIX/etc/yi_protect_client_go.key" \
    > /tmp/avclient.log 2>&1 &

# Talkback (speaker) receiver.
./talkback_rx > /tmp/talkback_rx.log 2>&1 &

# Watchdog.
WATCHDOG_INTERVAL=$WATCHDOG_INTERVAL "$YIP_PREFIX/script/watchdog.sh" > /tmp/watchdog.log 2>&1 &

exit 0
