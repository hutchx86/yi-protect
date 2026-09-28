#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the fully static armv7/musl HTTPS downloader (downloader.c + mbedTLS,
# fetched from upstream and checksum-verified, linked statically).
#
# Toolchain: the same sunxi-musl cross-toolchain as work/build_sd.sh (which
# calls this script). Standalone: set TCBIN to its bin/ directory, or
# TOOLCHAIN_DIR to a clone of lindenis-org/lindenis-v536-prebuilt.
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

TOOLCHAIN_DIR="${TOOLCHAIN_DIR:-$ROOT/repos/toolchain-sunxi-musl}"
TCBIN="${TCBIN:-$TOOLCHAIN_DIR/gcc/linux-x86/arm/toolchain-sunxi-musl/toolchain/bin}"
XP=arm-openwrt-linux-
CC="$TCBIN/${XP}gcc"
AR="$TCBIN/${XP}ar"
RANLIB="$TCBIN/${XP}ranlib"
[ -x "$CC" ] || { echo "ERROR: cross-compiler missing ($CC); set TCBIN or TOOLCHAIN_DIR"; exit 1; }
export STAGING_DIR="${STAGING_DIR:-$TCBIN/..}"

MBEDTLS_VER=2.28.8
MBEDTLS_SHA256=4fef7de0d8d542510d726d643350acb3cdb9dc76ad45611b59c9aa08372b4213
MBEDTLS_URL="https://github.com/Mbed-TLS/mbedtls/archive/refs/tags/v${MBEDTLS_VER}.tar.gz"
B="$HERE/build"
TGZ="$B/mbedtls-${MBEDTLS_VER}.tar.gz"
MB="$B/mbedtls-${MBEDTLS_VER}"

mkdir -p "$B"

# 1. Fetch + verify + extract mbedTLS (skipped if already present).
if [ ! -d "$MB" ]; then
    [ -f "$TGZ" ] || wget -q -O "$TGZ" "$MBEDTLS_URL" || curl -fsSL -o "$TGZ" "$MBEDTLS_URL"
    echo "$MBEDTLS_SHA256  $TGZ" | sha256sum -c - >/dev/null || \
        { echo "ERROR: $TGZ checksum mismatch"; exit 1; }
    tar -C "$B" -xzf "$TGZ"
fi

# 2. The three static mbedTLS libraries (skipped if already built).
if [ ! -f "$MB/library/libmbedcrypto.a" ]; then
    make -C "$MB" -j4 CC="$CC" AR="$AR" RANLIB="$RANLIB" \
        CFLAGS='-Os -ffunction-sections -fdata-sections' lib >/dev/null
fi

# 3. The downloader. -lc -lgcc_eh last: this toolchain's static libc.a
#    objects reference __aeabi_unwind_cpp_pr0, which only libgcc_eh.a has, and
#    gcc does not add libgcc_eh.a to a plain C link by itself.
"$CC" -Os -static -ffunction-sections -fdata-sections \
    -I "$MB/include" \
    -o "$HERE/downloader" "$HERE/downloader.c" \
    -Wl,--gc-sections -Wl,-s \
    -L "$MB/library" -lmbedtls -lmbedx509 -lmbedcrypto -lc -lgcc_eh
echo "built: $HERE/downloader"
