#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the self-contained SD-card layout (work/sd_root/) from source and
# package it as work/yi-protect-<rev>.tar.gz. Prerequisites are checked below.

set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SD="$ROOT/work/sd_root"
UNIFI="$SD/unifi"
BIN="$UNIFI/bin"
LIB="$UNIFI/lib"
ETC="$UNIFI/etc"
YH="$ROOT/repos/yi-hack-Allwinner-v2"
# Generic armv7-a hard-float musl cross-toolchain; the lindenis v536 and v833
# prebuilts ship the same compiler. Point TOOLCHAIN_DIR at a clone of either.
TOOLCHAIN_DIR="${TOOLCHAIN_DIR:-$ROOT/repos/toolchain-sunxi-musl}"
TCDIR="$TOOLCHAIN_DIR/gcc/linux-x86/arm/toolchain-sunxi-musl"
TCBIN="$TCDIR/toolchain/bin"
BUILD="$ROOT/work/build"
YHB="$BUILD/yi-hack"

mkdir -p "$BIN" "$LIB" "$ETC" "$UNIFI/script" "$BUILD"

export PATH="$TCBIN:$HOME/.local/bin:$PATH"
export STAGING_DIR="$TCDIR"

echo "== prerequisites =="
[ -x "$TCBIN/arm-openwrt-linux-gcc" ] || { echo "ERROR: cross-toolchain missing ($TCBIN). Clone lindenis-org/lindenis-v536-prebuilt into $TOOLCHAIN_DIR (or set TOOLCHAIN_DIR)."; exit 1; }
[ -d "$YH/src" ] || { echo "ERROR: yi-hack submodule missing ($YH). Run: git submodule update --init --recursive"; exit 1; }
command -v cmake >/dev/null 2>&1 || { echo "ERROR: cmake not found (needed for imggrabber). Try: python3 -m pip install --user cmake"; exit 1; }

XP=arm-openwrt-linux-
CC="${XP}gcc"; CXX="${XP}g++"; AR="${XP}ar"; STRIP="${XP}strip"

fetch() { [ -f "$2" ] || wget -q -O "$2" "$1"; }

# 1. Pristine yi-hack tree + patches; `git archive HEAD` (not the working tree)
#    keeps builds reproducible from the pinned commit + work/patches/*.
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
# /tmp/sd/yi-hack: ipc_cmd (model_suffix) and dropbear's host-key paths.
sed -i \
    -e 's|"/home/yi-hack/model_suffix"|"/tmp/sd/unifi/etc/model_suffix"|' \
    -e 's|"/tmp/sd/yi-hack/model_suffix"|"/tmp/sd/unifi/etc/model_suffix"|' \
    "$YHB/src/ipc_cmd/ipc_cmd/ptz.c"
sed -i 's|/tmp/sd/yi-hack|/tmp/sd/unifi|g' "$YHB/src/dropbear/localoptions.h"

# dropbear: a second password per account (the controller's device credential,
# SHA-512 crypt; work/dropbear/yp_extra_auth.c). Patched in right after
# init.dropbear unpacks the release tarball.
mkdir -p "$YHB/src/dropbear/yp"
cp "$ROOT"/work/dropbear/yp_crypt_sha512.c "$ROOT"/work/dropbear/yp_extra_auth.c \
   "$ROOT"/work/dropbear/svr-authpasswd.patch "$YHB/src/dropbear/yp/"
sed -i 's#^cp ../localoptions.h ./ || exit 1$#&\ncp ../yp/yp_*.c src/ \&\& patch -p1 -s < ../yp/svr-authpasswd.patch || exit 1#' \
    "$YHB/src/dropbear/init.dropbear"
grep -q 'svr-authpasswd.patch' "$YHB/src/dropbear/init.dropbear" || \
    { echo "ERROR: dropbear init.dropbear hook not applied"; exit 1; }

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

# 2. yi-hack-sourced components (each downloads + builds its own deps).
echo "== 2/6 yi-hack modules (ipc_cmd, set_tz_offset, dropbear, alsa-lib, snapshot) =="
build_module ipc_cmd
build_module set_tz_offset
build_module dropbear
build_module alsa-lib
build_module snapshot        # imggrabber: builds ffmpeg + libjpeg-turbo (slow)

# FAAD2 (GPL-2.0-or-later) static lib for the bridge's AAC->Opus transcode
# and talkback_rx's ADTS decode.
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

# 3. libopus from source (talkback_rx's RTP/Opus decoder).
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

# 4. This project's own code.
echo "== 4/6 our components (unifi_avclient_go, cpld_ctl, talkback_rx, unifi_flv_bridge, downloader) =="
( cd "$ROOT/work/goclient"
  CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7 CC="$CC" \
      go build -trimpath -ldflags="-s -w" -o "$BIN/unifi_avclient_go" . )

"$CC" -O2 -o "$BIN/cpld_ctl" "$ROOT/work/cpld_ctl/cpld_ctl.c"
"$STRIP" "$BIN/cpld_ctl"

# mkpasswd: MD5-crypt helper for unifi.cfg SSH_PASSWORD; the camera libc has
# no cryptpw applet/openssl, so init.sh needs our own crypt() call.
"$CC" -O2 -o "$BIN/mkpasswd" "$ROOT/work/mkpasswd/mkpasswd.c"
"$STRIP" "$BIN/mkpasswd"

# Static HTTPS downloader (prebuilt by work/downloader/build.sh): the camera has
# no https client/CA store, so init.sh uses it with -k to fetch the H.264 libs.
[ -x "$ROOT/work/downloader/downloader" ] || \
    { echo "ERROR: work/downloader/downloader missing (run work/downloader/build.sh)"; exit 1; }
cp "$ROOT/work/downloader/downloader" "$BIN/downloader"

# mixer_set: maps the controller's mic volume onto the codec capture element
# (analog of a real camera's UBNT_CVOLUME write); links the step-2 alsa-lib.
ALSASRC=$(printf '%s\n' "$YHB"/src/alsa-lib/alsa-lib-* | sed -n '1p')
"$CC" -O2 -Wall -o "$BIN/mixer_set" "$ROOT/work/mixer_set/mixer_set.c" \
    -I"$ALSASRC/include" "$YHB/src/alsa-lib/_install/lib/libasound.so.2" -lpthread
"$STRIP" "$BIN/mixer_set"

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

# 5. Collect yi-hack-built artifacts into the SD layout (from _install/).
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

# 6. Static assets: all-white blanks of the stock watermark size are bind-mounted
#    over the stock bitmaps in init.sh; no vendor watermark bitmap is shipped.
echo "== 6/6 assets =="
for b in main_blank.bmp sub_blank.bmp; do
    [ -f "$ETC/$b" ] || echo "   WARN: $b missing (watermark not hidden)"
done

# 6b. Per-model bring-up is generated on the camera at every boot from its own
#     stock lower_half_init.sh (unifi/script/gen-lower-half.sh); no vendor boot
#     script is shipped. Drop the per-model copies older builds left behind.
echo "== 6b/7 per-model bring-up: generated on camera (nothing to vendor) =="
MODEL_TABLE="$ETC/model_table"
if [ ! -f "$MODEL_TABLE" ]; then
    echo "ERROR: $MODEL_TABLE missing (it defines the supported models)"; exit 1
fi
LHDIR="${UNIFI:?}/script/lower_half"
for m in default $(awk '!/^[[:space:]]*#/ && NF {print $1}' "$MODEL_TABLE"); do
    rm -f "$LHDIR/$m.sh"
done
rmdir "$LHDIR" 2>/dev/null || true
[ -d "$LHDIR" ] && echo "   WARN: $LHDIR still holds files; it would ship in the tarball"

# 6c. License texts + source offer for our AGPL code and the compiled
#     GPL/LGPL third-party components.
cp "$ROOT/LICENSE" "$SD/LICENSE"
cp "$ROOT/NOTICE"  "$SD/NOTICE"
rm -rf "$SD/licenses"
cp -R "$ROOT/licenses" "$SD/licenses"
cat > "$SD/SOURCES.txt" <<SOURCES_EOF
Sources for the binaries in this image
======================================

This SD image contains this project's own code (AGPL-3.0-or-later) and
redistributes compiled third-party components. License texts are in LICENSE and
NOTICE. The project's own source is at
https://github.com/hutchx86/yi-protect . Corresponding source for the bundled
GPL/LGPL components:

* yi-hack-Allwinner-v2 (ipc_cmd/libipc, ipc_multiplex.so, imggrabber,
  set_tz_offset, dropbearmulti, patched alsa-lib)
  https://github.com/roleoroleo/yi-hack-Allwinner-v2            (GPL-3.0 / MIT)
* FFmpeg (static in imggrabber)       https://ffmpeg.org/       (LGPL-2.1)
* libjpeg-turbo (static in imggrabber)
  https://github.com/libjpeg-turbo/libjpeg-turbo                (BSD-3/IJG)
* FAAD2 ${FAAD2_VER} (static in unifi_flv_bridge, talkback_rx)
  https://github.com/knik0/faad2/releases/tag/${FAAD2_VER}      (GPL-2.0-or-later)
* libopus (static in unifi_flv_bridge, talkback_rx)
  https://opus-codec.org/                                       (BSD-2)
* alsa-lib (libasound.so.2)           https://www.alsa-project.org/  (LGPL-2.1)

Written offer: the yi-protect maintainers (https://github.com/hutchx86/yi-protect)
will provide the complete corresponding source for any GPL/LGPL component in
this image, on physical media or by download, to anyone who requests it. This
offer is valid for at least three years from the date of distribution.
SOURCES_EOF

# 7. Package the whole SD layout as one tarball; extract to the card root.
echo "== 7/7 packaging =="
# bin/ and lib/ are gitignored and never emptied, so anything left there by
# hand (e.g. a vendor-linked mediad from an old deploy) would ship. Refuse.
SHIP_BIN="cpld_ctl downloader dropbearmulti imggrabber ipc_cmd mixer_set mkpasswd set_tz_offset talkback_rx unifi_avclient_go unifi_flv_bridge"
SHIP_LIB="ipc_multiplex.so libasound.so.2"
stray=""
for f in "$BIN"/* "$LIB"/*; do
    [ -e "$f" ] || continue
    case " $SHIP_BIN $SHIP_LIB " in
        *" $(basename "$f") "*) ;;
        *) stray="$stray $f" ;;
    esac
done
if [ -n "$stray" ]; then
    echo "ERROR: files this script does not build are in the SD layout; move them out:"
    for f in $stray; do echo "   $f"; done
    exit 1
fi
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
