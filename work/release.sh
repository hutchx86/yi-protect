#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build the SD-card package and, optionally, publish it as a GitHub release.
#
#   work/release.sh                 # build; print artifact + checksum
#   PUBLISH=1 work/release.sh       # build; gh release create
#   TAG=v1.0.0 PUBLISH=1 work/release.sh

set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
sh "$ROOT/work/build_sd.sh"

REV=$(git -C "$ROOT" rev-parse --short HEAD)
PKG=$(ls -t "$ROOT"/work/yi-protect-*.tar.gz | head -1)
( cd "$ROOT/work" && sha256sum "$(basename "$PKG")" > SHA256SUMS )

echo "artifact: $PKG"
echo "checksum: $ROOT/work/SHA256SUMS"

if [ "${PUBLISH:-0}" = "1" ]; then
    TAG="${TAG:-v$REV}"
    command -v gh >/dev/null 2>&1 || { echo "gh not found; cannot publish"; exit 1; }
    gh release create "$TAG" "$PKG" "$ROOT/work/SHA256SUMS" \
        --title "yi-protect $REV" --generate-notes
fi
