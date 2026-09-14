#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# One-time install on a stock camera: patch /backup/init.sh to source
# /tmp/sd/lower_half_init.sh, then reboot:
#     sh /tmp/sd/unifi/install/install-backup-hook.sh
# No-op if the hook is already present. /backup is jffs2 (writable) and survives
# firmware updates. The appended block matches yi-hack's sdhack hook.

set -e

INIT=/backup/init.sh

if grep -q "/tmp/sd/lower_half_init.sh" "$INIT" 2>/dev/null; then
    echo "install-backup-hook: already installed, nothing to do"
    exit 0
fi

[ -w "$INIT" ] || { echo "install-backup-hook: $INIT not writable"; exit 1; }

echo "install-backup-hook: patching $INIT (backup kept at ${INIT}.unifi-orig)"
cp -f "$INIT" "${INIT}.unifi-orig"
cp -f "$INIT" /tmp/init.sh

# Remove the stock lower-half block (both whitespace variants), as yi-hack does.
sed -n '1{$!N;$!N;$!N;$!N};$!N;s@\nif \[ \-f \/home\/app\/lower_half_init.sh \];then\n    source \/home\/app\/lower_half_init.sh\nelse\n    source \/backup\/lower_half_init.sh\nfi@@;P;D' -i /tmp/init.sh
sed -n '1{$!N;$!N;$!N;$!N};$!N;s@\nif \[ \-f \/home\/app\/lower_half_init.sh \];then\n\tsource \/home\/app\/lower_half_init.sh\nelse\n\tsource \/backup\/lower_half_init.sh\nfi@@;P;D' -i /tmp/init.sh
sed -e 's/^source \/home\/app\/lower_half_init.sh//g' -i /tmp/init.sh

cat >> /tmp/init.sh <<'EOF'

# Running telnetd
/usr/sbin/telnetd &

if [ -f /tmp/sd/lower_half_init.sh ];then
    source /tmp/sd/lower_half_init.sh
elif [ -f /home/app/lower_half_init.sh ];then
    source /home/app/lower_half_init.sh
else
    source /backup/lower_half_init.sh
fi
EOF

cp -f /tmp/init.sh "$INIT"
sync

if cmp -s /tmp/init.sh "$INIT"; then
    echo "install-backup-hook: done -- reboot to boot from the SD card"
else
    echo "install-backup-hook: ERROR: write to $INIT failed (backup at ${INIT}.unifi-orig)"
    exit 1
fi
