#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Dump every raw MTD partition to the SD card for emergency recovery, before
# we modify the camera's boot. Idempotent: skips if a complete backup already
# exists unless --force is given.
#
# Output: /tmp/sd/yi-protect-backup/<model>/{mtdN_<name>.bin,md5sums,mtd.txt,
# RESTORE.md,restore.sh}

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
BKROOT="/tmp/sd/yi-protect-backup"
FORCE=0
[ "$1" = "--force" ] && FORCE=1

log() { echo "backup-flash: $*" >&2; }

if [ -f "$UNIFI_PREFIX/etc/model_suffix" ]; then
    MODEL=$(cat "$UNIFI_PREFIX/etc/model_suffix" 2>/dev/null)
fi
[ -z "$MODEL" ] && MODEL=$("$UNIFI_PREFIX/script/detect-model.sh" 2>/dev/null)
[ -z "$MODEL" ] && MODEL=unknown
DIR="$BKROOT/$MODEL"

if [ "$FORCE" -eq 0 ] && [ -f "$DIR/DONE" ]; then
    log "already backed up to $DIR (use --force to redo)"
    exit 0
fi

mkdir -p "$DIR" || { log "cannot create $DIR"; exit 1; }
cp /proc/mtd "$DIR/mtd.txt" 2>/dev/null

ok=0
bad=0
# /proc/mtd line: mtd0: <size> <erasesize> "name"
while read -r dev size erasesize name; do
    case "$dev" in mtd*:) ;; *) continue ;; esac
    [ "$size" = "00000000" ] && continue
    num=${dev%:}
    blk=${num#mtd}
    part=$(printf '%s' "$name" | tr -d '"')
    out="$DIR/${num}_${part}.bin"
    want=$((0x$size))
    if [ -s "$out" ]; then
        log "$num ($part) already dumped"
        ok=$((ok + 1))
        continue
    fi
    log "dumping $num ($part, $want bytes) -> $(basename "$out")"
    # mtdchar read() fails on this kernel; the mtdblock view reads cleanly.
    dd if="/dev/mtdblock$blk" of="$out" bs=4096 2>/dev/null
    got=$(ls -l "$out" 2>/dev/null | awk '{print $5}')
    if [ "$got" = "$want" ]; then
        ok=$((ok + 1))
    else
        log "FAILED $num (got ${got:-0}, want $want)"
        rm -f "$out"
        bad=$((bad + 1))
    fi
done < /proc/mtd

md5sum "$DIR"/*.bin > "$DIR/md5sums" 2>/dev/null

cat > "$DIR/RESTORE.md" <<'EOF'
Emergency flash restore
=======================

These are raw dumps of the camera's MTD partitions, taken before yi-protect
modified the boot. Model/layout specific: only restore onto the same model.

Each file maps to a `/proc/mtd` entry (see mtd.txt), named `mtdN_<name>.bin`.

WARNING: writing raw flash can brick the camera. Restoring `rootfs`, `home` or
`backup` while the system is running is unsafe. The safest path is U-Boot /
UART recovery, or a stock vendor flash tool. `restore.sh` is a last-resort
helper for a single partition from a running system; read it before using.

Verify dumps first:  md5sum -c md5sums
EOF

cat > "$DIR/restore.sh" <<'EOF'
#!/bin/sh
# DANGEROUS: writes raw flash. Same model/layout only. Read RESTORE.md first.
# Usage: ./restore.sh <mtdN>        (e.g. ./restore.sh mtd4)
set -e
[ -n "$1" ] || { echo "usage: $0 <mtdN>"; sed -n '3,6p' /proc/mtd; exit 2; }
part="$1"
file=$(ls "$(dirname "$0")/${part}"_*.bin 2>/dev/null | head -1)
[ -n "$file" ] || { echo "no dump for $part"; exit 2; }

# Refuse if the partition is currently mounted.
if grep -q "/dev/${part}\b" /proc/mounts 2>/dev/null; then
    echo "$part is mounted; refusing (unmount or use recovery mode)"; exit 1
fi
command -v flash_erase >/dev/null 2>&1 || { echo "flash_erase not available"; exit 1; }
command -v nandwrite  >/dev/null 2>&1 || { echo "nandwrite not available"; exit 1; }

echo "About to OVERWRITE /dev/$part with $file"
echo "This is irreversible. Type EXACTLY 'restore $part' to proceed:"
read -r ans
[ "$ans" = "restore $part" ] || { echo "aborted"; exit 1; }

flash_erase "/dev/$part" 0 0
nandwrite -p "/dev/$part" "$file"
echo "done; reboot when ready"
EOF
chmod +x "$DIR/restore.sh" 2>/dev/null

if [ "$bad" -eq 0 ] && [ "$ok" -gt 0 ]; then
    touch "$DIR/DONE"
    log "backup complete: $DIR ($ok partitions)"
    exit 0
fi
log "backup INCOMPLETE: $ok ok, $bad failed (no DONE marker)"
exit 1
