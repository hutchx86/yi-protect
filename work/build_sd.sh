#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the self-contained SD-card layout for the UniFi Protect emulation.
# Everything is built from source; nothing comes from a yi-hack install or a
# firmware dump.
#
# Output: work/sd_root/. Copy its CONTENTS to the root of a FAT32 SD card.
# The camera's patched /backup/init.sh sources /tmp/sd/lower_half_init.sh,
# which hands off to unifi/script/init.sh.
#
#   /tmp/sd/lower_half_init.sh        boot entry
#   /tmp/sd/unifi/bin/*               our binaries
#   /tmp/sd/unifi/lib/*               libasound.so.2, ipc_multiplex.so
#   /tmp/sd/unifi/etc/*               config + watermark assets
#   /tmp/sd/unifi/script/*            init, watchdog, detect-model, DHCP
#
# Prerequisites (checked below):
#   - git submodule: repos/yi-hack-Allwinner-v2
#   - repos/lindenis-v536-prebuilt (cross-toolchain), cloned manually
#   - cmake on PATH (libjpeg-turbo inside imggrabber):
#       python3 -m pip install --user cmake
#   - wget + network access to the upstream archives
#
# yi-hack is a pinned upstream build source. It is copied into work/build/ and
# its hardcoded /opt/yi/toolchain-sunxi-musl path rewritten to the toolchain
# submodule before the per-module init/compile scripts run. The AAC decoder is
# FAAD2 (GPL-2.0-or-later), fetched and built from source like libopus.

set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SD="$ROOT/work/sd_root"
UNIFI="$SD/unifi"
BIN="$UNIFI/bin"
LIB="$UNIFI/lib"
ETC="$UNIFI/etc"
YH="$ROOT/repos/yi-hack-Allwinner-v2"
TCDIR="$ROOT/repos/lindenis-v536-prebuilt/gcc/linux-x86/arm/toolchain-sunxi-musl"
TCBIN="$TCDIR/toolchain/bin"
BUILD="$ROOT/work/build"
YHB="$BUILD/yi-hack"

mkdir -p "$BIN" "$LIB" "$ETC" "$UNIFI/script" "$BUILD"

export PATH="$TCBIN:$HOME/.local/bin:$PATH"
export STAGING_DIR="$TCDIR"

echo "== prerequisites =="
[ -x "$TCBIN/arm-openwrt-linux-gcc" ] || { echo "ERROR: toolchain missing ($TCBIN). Run: git submodule update --init --recursive"; exit 1; }
[ -d "$YH/src" ] || { echo "ERROR: yi-hack submodule missing ($YH). Run: git submodule update --init --recursive"; exit 1; }
command -v cmake >/dev/null 2>&1 || { echo "ERROR: cmake not found (needed for imggrabber). Try: python3 -m pip install --user cmake"; exit 1; }

XP=arm-openwrt-linux-
CC="${XP}gcc"; CXX="${XP}g++"; AR="${XP}ar"; STRIP="${XP}strip"

fetch() { [ -f "$2" ] || wget -q -O "$2" "$1"; }

# ---------------------------------------------------------------------------
# 1. Pristine yi-hack tree + patches. `git archive HEAD` (not the working
#    tree) keeps builds reproducible from the pinned commit + work/patches/*.
# ---------------------------------------------------------------------------
echo "== 1/6 preparing yi-hack build tree =="
rm -rf "$YHB"
mkdir -p "$YHB"
git -C "$YH" archive HEAD src scripts | tar -x -C "$YHB"
if [ -d "$ROOT/work/patches" ]; then
    for p in "$ROOT"/work/patches/*.patch; do
        [ -f "$p" ] || continue
        echo "   applying $(basename "$p")"
        patch -p1 -d "$YHB" -s < "$p"
    done
fi
grep -rl "/opt/yi/toolchain-sunxi-musl" "$YHB" 2>/dev/null | while read -r f; do
    sed -i "s|/opt/yi/toolchain-sunxi-musl|$TCDIR|g" "$f"
done

# Repath compiled-in yi-hack paths so the shipped binaries never read
# /tmp/sd/yi-hack at runtime: ipc_cmd (model_suffix) and dropbear's host-key
# paths (a missing key path broke the SSH KEX).
sed -i \
    -e 's|"/home/yi-hack/model_suffix"|"/tmp/sd/unifi/etc/model_suffix"|' \
    -e 's|"/tmp/sd/yi-hack/model_suffix"|"/tmp/sd/unifi/etc/model_suffix"|' \
    "$YHB/src/ipc_cmd/ipc_cmd/ptz.c"
sed -i 's|/tmp/sd/yi-hack|/tmp/sd/unifi|g' "$YHB/src/dropbear/localoptions.h"

build_module() {
    name="$1"
    d="$YHB/src/$name"
    [ -d "$d" ] || { echo "ERROR: no yi-hack module '$name'"; exit 1; }
    echo "   building yi-hack:$name"
    ( cd "$d"
      [ -x "./init.$name" ]    && "./init.$name"
      [ -x "./compile.$name" ] && "./compile.$name"
      true )
}

# ---------------------------------------------------------------------------
# 2. yi-hack-sourced components (each downloads + builds its own deps).
# ---------------------------------------------------------------------------
echo "== 2/6 yi-hack modules (ipc_cmd, set_tz_offset, dropbear, alsa-lib, snapshot) =="
build_module ipc_cmd
build_module set_tz_offset
build_module dropbear
build_module alsa-lib
build_module snapshot        # imggrabber: builds ffmpeg + libjpeg-turbo (slow)

# AAC decoder (FAAD2, GPL-2.0-or-later), static lib for unifi_flv_bridge's
# AAC->Opus transcode and talkback_rx's ADTS decode.
FAAD2_VER=2.11.3
build_faad2() {
    d="$BUILD/faad2-$FAAD2_VER"
    if [ -f "$d/libfaad.a" ]; then return 0; fi
    echo "   building faad2-$FAAD2_VER"
    fetch "https://github.com/knik0/faad2/archive/refs/tags/${FAAD2_VER}.tar.gz" \
          "$BUILD/faad2-${FAAD2_VER}.tar.gz"
    rm -rf "$BUILD/faad2-src"
    mkdir -p "$BUILD/faad2-src"
    tar xf "$BUILD/faad2-${FAAD2_VER}.tar.gz" -C "$BUILD/faad2-src" --strip-components=1
    # Cross-compile only the static float library target (no frontend/DRM/fixed).
    cmake -S "$BUILD/faad2-src" -B "$d" \
        -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=arm \
        -DCMAKE_C_COMPILER="$TCBIN/$CC" \
        -DCMAKE_AR="$TCBIN/$AR" \
        -DCMAKE_RANLIB="$TCBIN/${XP}ranlib" \
        -DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY \
        -DBUILD_SHARED_LIBS=OFF -DCMAKE_BUILD_TYPE=Release >/dev/null
    cmake --build "$d" --target faad -j4 >/dev/null
}
build_faad2

# ---------------------------------------------------------------------------
# 3. libopus from source (talkback_rx's RTP/Opus decoder).
# ---------------------------------------------------------------------------
echo "== 3/6 libopus =="
OPUS_VER=1.5.2
OPUS_DIR="$BUILD/opus-$OPUS_VER"
if [ ! -f "$OPUS_DIR/.libs/libopus.a" ]; then
    ( cd "$BUILD"
      fetch "https://downloads.xiph.org/releases/opus/opus-$OPUS_VER.tar.gz" "opus-$OPUS_VER.tar.gz"
      [ -d "opus-$OPUS_VER" ] || tar xf "opus-$OPUS_VER.tar.gz"
      cd "opus-$OPUS_VER"
      ./configure --host=arm-openwrt-linux --disable-shared --enable-static \
          --disable-doc --disable-extra-programs >/dev/null
      make -j4 >/dev/null )
fi

# ---------------------------------------------------------------------------
# 4. This project's own code.
# ---------------------------------------------------------------------------
echo "== 4/6 our components (unifi_avclient_go, cpld_ctl, talkback_rx, unifi_flv_bridge) =="
( cd "$ROOT/work/goclient"
  CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7 CC="$CC" \
      go build -ldflags="-s -w" -o "$BIN/unifi_avclient_go" . )

"$CC" -O2 -o "$BIN/cpld_ctl" "$ROOT/work/cpld_ctl/cpld_ctl.c"
"$STRIP" "$BIN/cpld_ctl"

FAAD2="$BUILD/faad2-$FAAD2_VER"

# Our FlvPush/FshareReader bridge; FAAD2/OPUS feed its AAC->Opus transcode.
( cd "$ROOT/work/flv_bridge"
  make -s clean
  make -s CXX="$CXX" CXXFLAGS="-O2 -Wall -std=gnu++14" \
      FAAD2="$FAAD2" OPUS="$OPUS_DIR" unifi_flv_bridge
  "$STRIP" unifi_flv_bridge
  cp unifi_flv_bridge "$BIN/unifi_flv_bridge" )

( cd "$ROOT/work/talkback/rx"
  "$CC" -O2 -o "$BIN/talkback_rx" talkback_rx.c \
      -I"$FAAD2/include" -I"$OPUS_DIR/include" \
      "$FAAD2/libfaad.a" "$OPUS_DIR/.libs/libopus.a" -lm )
"$STRIP" "$BIN/talkback_rx"

# ---------------------------------------------------------------------------
# 5. Collect yi-hack-built artifacts into the SD layout (from _install/).
# ---------------------------------------------------------------------------
echo "== 5/6 installing built artifacts =="
I="$YHB/src"
cp "$I/ipc_cmd/_install/bin/ipc_cmd"              "$BIN/"
cp "$I/ipc_cmd/_install/lib/ipc_multiplex.so"     "$LIB/"
cp "$I/set_tz_offset/_install/bin/set_tz_offset"  "$BIN/"
cp "$I/dropbear/_install/dropbearmulti"           "$BIN/"
# libasound named .so.2 to match its SONAME (FAT32 can't hold the .so.2 -> .so.2.0.0 symlink)
cp "$I/alsa-lib/_install/lib/libasound.so.2.0.0"  "$LIB/libasound.so.2"
cp "$I/snapshot/_install/bin/imggrabber"          "$BIN/"
"$STRIP" "$BIN/dropbearmulti" "$LIB/libasound.so.2" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 6. Static assets.
# ---------------------------------------------------------------------------
echo "== 6/6 assets =="
# The stock Yi watermark is hidden by bind-mounting all-white blanks of the
# same size (unifi/etc/{main,sub}_blank.bmp, tracked) over the stock bitmaps in
# init.sh. No vendor-derived watermark bitmap is shipped.
for b in main_blank.bmp sub_blank.bmp; do
    [ -f "$ETC/$b" ] || echo "   WARN: $b missing (watermark not hidden)"
done

# ---------------------------------------------------------------------------
# 6b. Vendor the per-model bring-up scripts: stock/yi-hack lower_half_init.sh
#     with /tmp/sd/yi-hack repathed to /tmp/sd/unifi. lower_half_init.sh picks
#     one by the auto-detected model, so one image boots any supported model.
# ---------------------------------------------------------------------------
echo "== 6b/7 vendoring per-model bring-up =="
# Only the models we actually own/support; add more here as they are brought up.
SUPPORTED_MODELS="${SUPPORTED_MODELS:-y623 h52ga r35gb}"
LHDIR="$UNIFI/script/lower_half"
mkdir -p "$LHDIR"
for m in $SUPPORTED_MODELS; do
    f="$YH/sysroot/$m/lower_half_init.sh"
    [ -f "$f" ] || { echo "   WARN: no bring-up script for $m (skipped)"; continue; }
    sed -e 's#/tmp/sd/yi-hack/script/system.sh#/tmp/sd/unifi/script/init.sh#g' \
        -e 's#/tmp/sd/yi-hack/script/wifidhcp.sh#/tmp/sd/unifi/script/wifidhcp.sh#g' \
        -e 's#/tmp/sd/yi-hack/script/ethdhcp.sh#/tmp/sd/unifi/script/ethdhcp.sh#g' \
        -e 's#/tmp/sd/yi-hack/lib/ipc_multiplex.so#/tmp/sd/unifi/lib/ipc_multiplex.so#g' \
        -e 's#/tmp/sd/yi-hack/etc/system.conf#/tmp/sd/unifi/etc/unifi.cfg#g' \
        -e 's#/tmp/sd/yi-hack/etc/watermark/blank.bmp#/tmp/sd/unifi/etc/main_blank.bmp#g' \
        "$f" > "$LHDIR/$m.sh"
done
cp "$LHDIR/y623.sh" "$LHDIR/default.sh" 2>/dev/null || true
echo "   lower_half models: $(ls "$LHDIR" | tr '\n' ' ')"

# ---------------------------------------------------------------------------
# 6c. License texts + source offer: the image redistributes compiled GPL/LGPL
#     components, so ship the texts and a pointer to their source.
# ---------------------------------------------------------------------------
cp "$ROOT/LICENSE" "$SD/LICENSE"
cp "$ROOT/NOTICE"  "$SD/NOTICE"
cat > "$SD/SOURCES.txt" <<'SOURCES_EOF'
Sources for the binaries in this image
======================================

This SD image redistributes compiled third-party components. License texts are
in LICENSE and NOTICE. Corresponding source for the GPL/LGPL components:

* yi-hack-Allwinner-v2 (ipc_cmd/libipc, ipc_multiplex.so, imggrabber,
  set_tz_offset, dropbearmulti, patched alsa-lib)
  https://github.com/roleoroleo/yi-hack-Allwinner-v2            (GPL-3.0 / MIT)
* FFmpeg (static in imggrabber)       https://ffmpeg.org/       (LGPL-2.1)
* libjpeg-turbo (static in imggrabber)
  https://github.com/libjpeg-turbo/libjpeg-turbo                (BSD-3/IJG)
* FAAD2 (static in unifi_flv_bridge, talkback_rx)
  https://github.com/knik0/faad2                               (GPL-2.0-or-later)
* libopus (static in unifi_flv_bridge, talkback_rx)
  https://opus-codec.org/                                       (BSD-2)
* alsa-lib (libasound.so.2)           https://www.alsa-project.org/  (LGPL-2.1)

Written offer: the maintainers will provide the complete corresponding source
for any GPL/LGPL component in this image on request.
SOURCES_EOF

# ---------------------------------------------------------------------------
# 7. Single downloadable package: the whole SD layout in one tarball. Extract
#    its contents to the SD card root and boot.
# ---------------------------------------------------------------------------
echo "== 7/7 packaging =="
REV=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)
PKG="$ROOT/work/yi-protect-$REV.tar.gz"
rm -f "$PKG"
( cd "$SD" && tar czf "$PKG" . )
echo "   $PKG ($(du -h "$PKG" | cut -f1))"

echo ""
echo "Done. SD layout under $SD"
echo "  bin/: $(ls "$BIN" | tr '\n' ' ')"
echo "  lib/: $(ls "$LIB" | tr '\n' ' ')"
echo "  package: $PKG"
echo "Extract the package (or copy the CONTENTS of $SD) to the SD card root and boot."
