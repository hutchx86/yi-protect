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

# Optional clean-room mediad (sister project yi-mediad). When this dist exists it
# is packaged into the image so a user can opt into the advanced encoder path
# with unifi.cfg IS_MEDIAD=yes; without it the image is built rmm-only (still
# fully functional, just no H.265/advanced picture controls).
MEDIAD_DIST="${MEDIAD_DIST:-$ROOT/../sisters/yi-mediad/yi-mediad-git/dist}"

mkdir -p "$BIN" "$LIB" "$ETC" "$UNIFI/script" "$BUILD"

export PATH="$TCBIN:$HOME/.local/bin:$PATH"
export STAGING_DIR="$TCDIR"

echo "== prerequisites =="
[ -x "$TCBIN/arm-openwrt-linux-gcc" ] || { echo "ERROR: cross-toolchain missing ($TCBIN). Clone lindenis-org/lindenis-v536-prebuilt into $TOOLCHAIN_DIR (or set TOOLCHAIN_DIR)."; exit 1; }
[ -d "$YH/src" ] || { echo "ERROR: yi-hack submodule missing ($YH). Run: git submodule update --init --recursive"; exit 1; }
command -v cmake >/dev/null 2>&1 || { echo "ERROR: cmake not found (needed for libjpeg-turbo). Try: python3 -m pip install --user cmake"; exit 1; }

XP=arm-openwrt-linux-
CC="${XP}gcc"; CXX="${XP}g++"; AR="${XP}ar"; STRIP="${XP}strip"

fetch() { [ -f "$2" ] || wget -q -O "$2" "$1"; }

# 1. Pristine yi-hack tree; `git archive HEAD` (not the working tree) keeps
#    builds reproducible from the pinned commit.
echo "== 1/6 preparing yi-hack build tree =="
rm -rf "$YHB"
mkdir -p "$YHB"
git -C "$YH" archive HEAD src scripts | tar -x -C "$YHB"
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

# dropbear: second per-account password (controller device credential, SHA-512
# crypt; work/dropbear/yp_extra_auth.c), patched in after the tarball unpacks.
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
echo "== 2/6 yi-hack modules (ipc_cmd, set_tz_offset, dropbear, alsa-lib) =="
build_module ipc_cmd
build_module set_tz_offset
build_module dropbear
build_module alsa-lib

# 2b. Snapshot decode/encode deps for our own unifi_snapshot: FFmpeg (H.264
#     decoder) + libjpeg-turbo, built from upstream (no yi-hack imggrabber).
echo "== 2b/6 snapshot deps (ffmpeg, libjpeg-turbo) =="
SNAPD="$BUILD/snapshot-deps"
FFMPEG_VER=8.1.1
FFMPEG_DIR="$SNAPD/ffmpeg-$FFMPEG_VER"
JPEGLIB_VER=3.1.4.1
JPEGLIB_DIR="$SNAPD/libjpeg-turbo-$JPEGLIB_VER"
JPEG_DIR="$SNAPD/libjpeg"
mkdir -p "$SNAPD"
if [ ! -f "$FFMPEG_DIR/libavcodec/libavcodec.a" ]; then
    fetch "https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VER.tar.bz2" "$SNAPD/ffmpeg-$FFMPEG_VER.tar.bz2"
    [ -d "$FFMPEG_DIR" ] || tar xf "$SNAPD/ffmpeg-$FFMPEG_VER.tar.bz2" -C "$SNAPD"
    ( cd "$FFMPEG_DIR"
      ./configure --enable-cross-compile --cross-prefix="$TCBIN/$XP" \
          --arch=arm --target-os=linux --enable-thumb --enable-small \
          --disable-autodetect --disable-ffplay --disable-ffprobe --disable-doc \
          --disable-decoders --enable-decoder=h264,hevc --disable-encoders \
          --disable-demuxers --disable-muxers --disable-protocols \
          --disable-parsers --enable-parser=h264,hevc \
          --disable-filters --disable-bsfs --disable-indevs --disable-outdevs \
          --disable-swscale \
          --extra-cflags="-Os -ffunction-sections -fdata-sections" \
          > "$SNAPD/ffmpeg-configure.log" 2>&1
      make -j4 > "$SNAPD/ffmpeg-make.log" 2>&1 )
fi
if [ ! -f "$JPEG_DIR/lib/libjpeg.a" ]; then
    fetch "https://github.com/libjpeg-turbo/libjpeg-turbo/archive/refs/tags/$JPEGLIB_VER.tar.gz" "$SNAPD/jpeg.tar.gz"
    [ -d "$JPEGLIB_DIR" ] || tar xzf "$SNAPD/jpeg.tar.gz" -C "$SNAPD"
    cmake -S "$JPEGLIB_DIR" -B "$JPEGLIB_DIR/build" \
        -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=arm \
        -DCMAKE_C_COMPILER="$TCBIN/${XP}gcc" \
        -DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY \
        -DENABLE_SHARED=OFF -DWITH_SIMD=OFF -DWITH_TURBOJPEG=OFF -DWITH_JPEG8=1 \
        -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$JPEG_DIR" \
        > "$SNAPD/jpeg-cmake.log" 2>&1
    cmake --build "$JPEGLIB_DIR/build" --target install -j4 \
        > "$SNAPD/jpeg-make.log" 2>&1
fi

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

# Static HTTPS downloader (downloader.c + mbedTLS): the camera has no https
# client/CA store, so init.sh uses it with -k to fetch the md5-checked H.264 libs.
TCBIN="$TCBIN" sh "$ROOT/work/downloader/build.sh"
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

# unifi_snapshot: Protect GetRequest JPEG from the ring's keyframe (our own
# replacement for yi-hack's imggrabber); reuses the bridge's ring reader.
( cd "$ROOT/work/snapshot"
  make -s clean
  make -s CXX="$CXX" CXXFLAGS="-O2 -Wall -std=gnu++14" \
      FLV="$ROOT/work/flv_bridge" FFMPEG_DIR="$FFMPEG_DIR" JPEG_DIR="$JPEG_DIR" \
      unifi_snapshot
  "$STRIP" unifi_snapshot
  cp unifi_snapshot "$BIN/unifi_snapshot" )

# 5. Collect yi-hack-built artifacts into the SD layout (from _install/).
echo "== 5/6 installing built artifacts =="
I="$YHB/src"
cp "$I/ipc_cmd/_install/bin/ipc_cmd"              "$BIN/"
cp "$I/ipc_cmd/_install/lib/ipc_multiplex.so"     "$LIB/"
cp "$I/set_tz_offset/_install/bin/set_tz_offset"  "$BIN/"
cp "$I/dropbear/_install/dropbearmulti"           "$BIN/"
# libasound named .so.2 to match its SONAME (FAT32 can't hold the .so.2 -> .so.2.0.0 symlink)
cp "$I/alsa-lib/_install/lib/libasound.so.2.0.0"  "$LIB/libasound.so.2"
"$STRIP" "$BIN/dropbearmulti" "$LIB/libasound.so.2" 2>/dev/null || true

# 5b. Optional clean-room mediad (sister project yi-mediad): the advanced encoder
#     path, selected at boot by unifi.cfg IS_MEDIAD=yes (init.sh/watchdog.sh
#     already branch on it). Its libs are self-contained (no vendor .so fetch).
echo "== 5b/7 mediad (optional advanced encoder) =="
if [ -x "$MEDIAD_DIST/unifi/bin/mediad" ]; then
    cp "$MEDIAD_DIST/unifi/bin/mediad" "$BIN/mediad"
    cp "$MEDIAD_DIST/unifi/script/mediad.sh" "$UNIFI/script/mediad.sh"
    [ -f "$MEDIAD_DIST/unifi/etc/mediad.conf" ] && cp "$MEDIAD_DIST/unifi/etc/mediad.conf" "$ETC/mediad.conf"
    # The dist is the source of truth: drop stale env copies left in the
    # gitignored sd_root by an earlier build, then copy the dist's (if any).
    rm -f "$ETC"/mediad.env "$ETC"/mediad.*.env
    for env in "$MEDIAD_DIST"/unifi/etc/mediad.*.env; do
        [ -f "$env" ] && cp "$env" "$ETC/$(basename "$env")"
    done
    for lib in "$MEDIAD_DIST"/unifi/lib/*.so; do
        [ -f "$lib" ] && cp "$lib" "$LIB/"
    done
    echo "   mediad packaged from $MEDIAD_DIST"
else
    echo "   NOTE: no mediad dist at $MEDIAD_DIST; building an rmm-only image."
fi

# 6. Static assets: all-white blanks of the stock watermark size are bind-mounted
#    over the stock bitmaps in init.sh; no vendor watermark bitmap is shipped.
echo "== 6/6 assets =="
for b in main_blank.bmp sub_blank.bmp; do
    [ -f "$ETC/$b" ] || echo "   WARN: $b missing (watermark not hidden)"
done

# 6b. Per-model bring-up is generated on camera at boot from its stock
#     lower_half_init.sh (gen-lower-half.sh); drop stale per-model copies.
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
# mediad's own third-party notices (AGPL clean-room, Terminus font, FAAC, ...).
if [ -d "$MEDIAD_DIST/licenses" ]; then
    cp -R "$MEDIAD_DIST/licenses" "$SD/licenses/mediad"
fi
cat > "$SD/SOURCES.txt" <<SOURCES_EOF
Sources for the binaries in this image
======================================

This SD image contains this project's own code (AGPL-3.0-or-later) and
redistributes compiled third-party components. License texts are in LICENSE,
NOTICE and licenses/. The project's own source is at
https://github.com/hutchx86/yi-protect . Components shipped in this image:

* yi-hack-Allwinner-v2 (pinned submodule commit; ipc_cmd, ipc_multiplex.so,
  set_tz_offset, dropbearmulti, patched alsa-lib)
  https://github.com/roleoroleo/yi-hack-Allwinner-v2          (MIT / GPL-3.0)
* libipc (ipc_cmd, ipc_multiplex.so)
  https://github.com/TheCrypt0/libipc                         (GPL-3.0)
* FFmpeg 8.1.1 (static in unifi_snapshot)
  https://ffmpeg.org/releases/ffmpeg-8.1.1.tar.bz2            (LGPL-2.1)
* libjpeg-turbo 3.1.4.1 (static in unifi_snapshot)
  https://github.com/libjpeg-turbo/libjpeg-turbo              (BSD-3-Clause / IJG)
* FAAD2 ${FAAD2_VER} (static in unifi_flv_bridge, talkback_rx)
  https://github.com/knik0/faad2/releases/tag/${FAAD2_VER}    (GPL-2.0-or-later)
* libopus ${OPUS_VER} (static in unifi_flv_bridge, talkback_rx)
  https://downloads.xiph.org/releases/opus/opus-${OPUS_VER}.tar.gz (BSD-3-Clause)
* alsa-lib 1.1.4.1 + yi-hack audio-FIFO patch (libasound.so.2)
  https://www.alsa-project.org/files/pub/lib/alsa-lib-1.1.4.1.tar.bz2 (LGPL-2.1)
* Dropbear 2026.91 + work/dropbear patch (dropbearmulti)
  https://github.com/mkj/dropbear/releases/tag/DROPBEAR_2026.91 (MIT; patch AGPL-3.0-or-later)
* musl libc (static in downloader; SHA-512 crypt in dropbearmulti)
  https://musl.libc.org/                                      (MIT)
* Mbed TLS 2.28.8 (static in downloader)
  https://github.com/Mbed-TLS/mbedtls/releases/tag/v2.28.8    (Apache-2.0)
* gorilla/websocket v1.5.3 (in unifi_avclient_go)
  https://github.com/gorilla/websocket                        (BSD-2-Clause)
* Go runtime/standard library (in unifi_avclient_go; go.mod go 1.23.4,
  release CI builds with Go 1.23.x)  https://go.dev/            (BSD-3-Clause)

Written offer: the yi-protect maintainers (https://github.com/hutchx86/yi-protect)
will provide the complete corresponding source for any GPL/LGPL component in
this image, on physical media or by download, to anyone who requests it. This
offer is valid for at least three years from the date of distribution.
SOURCES_EOF

# mediad is optional; when packaged, name its sources and the exact build commits
# (the sister project writes its own authoritative SOURCE.txt into licenses/mediad).
if [ -x "$BIN/mediad" ]; then
    cat >> "$SD/SOURCES.txt" <<MEDIAD_EOF

* mediad -- clean-room drop-in media daemon (H.264 + H.265), AGPL-3.0-only with
  a section 7 linker exception (licenses/mediad/LICENSE-EXCEPTION). Its linked
  clean-room tiers and FAAC; exact commits in licenses/mediad/SOURCE.txt:
    yi-mediad   https://github.com/hutchx86/yi-mediad      (AGPL-3.0-only)
    freewinner  https://github.com/hutchx86/freewinner     (AGPL-3.0-only)
    freecodec   https://github.com/hutchx86/freecodec      (AGPL-3.0-only)
    FAAC        https://github.com/knik0/faac              (LGPL-2.1-or-later)
  Terminus Bold font, burned into mediad's OSD:
    https://terminus-font.sourceforge.net                  (OFL-1.1)
MEDIAD_EOF
fi

# 7. Package the whole SD layout as one tarball; extract to the card root.
echo "== 7/7 packaging =="
# bin/ and lib/ are gitignored and never emptied, so anything left there by
# hand (e.g. a vendor-linked mediad from an old deploy) would ship. Refuse.
SHIP_BIN="cpld_ctl downloader dropbearmulti ipc_cmd mediad mixer_set mkpasswd set_tz_offset talkback_rx unifi_avclient_go unifi_flv_bridge unifi_snapshot"
SHIP_LIB="ipc_multiplex.so libasound.so.2 libvenc_base.so vin_crop_shim.so"
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

# The image must be camera-universal. No symlinks (FAT32 cannot store them), and
# no per-camera identity: device-id, TLS cert/key, adopt state, dropbear host
# keys and model_suffix are all generated on the camera at boot, so if one is
# left in the tree (e.g. the card was booted before repackaging) it would clone
# that unit's identity into every image.
links=$(find "$SD" -type l 2>/dev/null)
if [ -n "$links" ]; then
    echo "ERROR: symlinks in the SD layout (FAT32 cannot hold them):"
    for l in $links; do echo "   $l"; done
    exit 1
fi

ETC_ALLOW="configure_wifi.cfg.example model_table main_blank.bmp sub_blank.bmp unifi.cfg mediad.conf"
for f in "$ETC"/*; do
    [ -e "$f" ] || continue
    _bn=$(basename "$f")
    case " $ETC_ALLOW " in
        *" $_bn "*) ;;
        *)
            # Per-model/per-deploy mediad knobs (mediad.env, mediad.<model>.env).
            case "$_bn" in mediad.env|mediad.*.env) ;; *)
                echo "ERROR: unexpected file in unifi/etc (per-camera identity leak?): $f"; exit 1 ;;
            esac
            ;;
    esac
done

for pat in unifi_client_go.device-id unifi_client_go.crt unifi_client_go.key \
           unifi_client_go.adoption-uuid unifi_client_go.adopted \
           unifi_client_go.device-name 'unifi_client_go.inform-host*' \
           model_suffix configure_wifi.cfg '*.applied' 'dropbear_*_host_key'; do
    found=$(find "$SD" -name "$pat" 2>/dev/null)
    if [ -n "$found" ]; then
        echo "ERROR: per-camera runtime identity present in the SD layout:"
        for f in $found; do echo "   $f"; done
        exit 1
    fi
done

if [ -d "$SD/unifi/log" ]; then
    echo "ERROR: $SD/unifi/log exists (runtime debug output); remove it before packaging"
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
