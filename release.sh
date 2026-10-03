#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the SD-card package (and its corresponding source) and optionally publish
# it as a GitHub release:
#   release.sh | NAME=v0.2.0 release.sh | NAME=v0.2.0 PUBLISH=1 release.sh
#
# NAME defaults to the short git revision; the release CI sets it to the release
# tag so the assets are yi-protect-<tag>.tar.gz / yi-protect-<tag>-src.tar.gz.

set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"
REPOS_DIR="${REPOS_DIR:-$ROOT/../repos}"
NAME="${NAME:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)}"
export NAME
sh "$ROOT/build_sd.sh"

PKG="$ROOT/yi-protect-$NAME.tar.gz"
[ -f "$PKG" ] || { echo "ERROR: expected $PKG missing"; exit 1; }

# Complete corresponding source for our AGPL project and the image's GPL/LGPL
# components; anything not captured here is covered by the SOURCES.txt offer.
SRC="$ROOT/yi-protect-$NAME-src.tar.gz"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT INT TERM
mkdir -p "$STAGE/yi-protect" "$STAGE/yi-hack-Allwinner-v2" "$STAGE/upstream"
git -C "$ROOT" archive HEAD | tar -x -C "$STAGE/yi-protect"
if git -C "$REPOS_DIR/yi-hack-Allwinner-v2" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "$REPOS_DIR/yi-hack-Allwinner-v2" archive HEAD | tar -x -C "$STAGE/yi-hack-Allwinner-v2"
fi
find "$ROOT/build" -maxdepth 6 -type f \
    \( -name '*.tar.gz' -o -name '*.tar.xz' -o -name '*.tar.bz2' -o -name '*.tgz' \) \
    -exec cp -n {} "$STAGE/upstream/" \; 2>/dev/null || true
( cd "$STAGE" && tar czf "$SRC" . )

( cd "$ROOT" && sha256sum "$(basename "$PKG")" "$(basename "$SRC")" > SHA256SUMS )

echo "artifact: $PKG"
echo "sources:  $SRC"
echo "checksum: $ROOT/SHA256SUMS"

if [ "${PUBLISH:-0}" = "1" ]; then
    TAG="${TAG:-v$NAME}"
    command -v gh >/dev/null 2>&1 || { echo "gh not found; cannot publish"; exit 1; }
    gh release create "$TAG" "$PKG" "$SRC" "$ROOT/SHA256SUMS" --title "yi-protect"
fi
