#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the SD-card package and, optionally, publish it as a GitHub release.
# Also bundles the corresponding source for this AGPL project and the
# redistributed GPL/LGPL components, so the release meets its source
# obligations.
#
#   work/release.sh                 # build; print artifact + checksum
#   PUBLISH=1 work/release.sh       # build; gh release create
#   TAG=v1.0.0 PUBLISH=1 work/release.sh

set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
sh "$ROOT/work/build_sd.sh"

REV=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)
PKG="$ROOT/work/yi-protect-$REV.tar.gz"
[ -f "$PKG" ] || { echo "ERROR: expected $PKG missing"; exit 1; }

# Complete corresponding source for this AGPL project and the GPL/LGPL
# components in the image. Anything not captured here is covered by the written
# offer in the image's SOURCES.txt.
SRC="$ROOT/work/yi-protect-$REV-src.tar.gz"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT INT TERM
mkdir -p "$STAGE/yi-protect" "$STAGE/yi-hack-Allwinner-v2" "$STAGE/upstream"
git -C "$ROOT" archive HEAD | tar -x -C "$STAGE/yi-protect"
if git -C "$ROOT/repos/yi-hack-Allwinner-v2" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "$ROOT/repos/yi-hack-Allwinner-v2" archive HEAD | tar -x -C "$STAGE/yi-hack-Allwinner-v2"
fi
find "$ROOT/work/build" -maxdepth 6 -type f \
    \( -name '*.tar.gz' -o -name '*.tar.xz' -o -name '*.tar.bz2' -o -name '*.tgz' \) \
    -exec cp -n {} "$STAGE/upstream/" \; 2>/dev/null || true
( cd "$STAGE" && tar czf "$SRC" . )

( cd "$ROOT/work" && sha256sum "$(basename "$PKG")" "$(basename "$SRC")" > SHA256SUMS )

echo "artifact: $PKG"
echo "sources:  $SRC"
echo "checksum: $ROOT/work/SHA256SUMS"

if [ "${PUBLISH:-0}" = "1" ]; then
    TAG="${TAG:-v$REV}"
    command -v gh >/dev/null 2>&1 || { echo "gh not found; cannot publish"; exit 1; }
    gh release create "$TAG" "$PKG" "$SRC" "$ROOT/work/SHA256SUMS" \
        --title "yi-protect $REV" --generate-notes
fi
