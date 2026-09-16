#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# First-boot installer, run by the stock firmware's factory hook when
# /tmp/sd/Factory/factory_test.sh exists:
#   if [ -f /tmp/sd/Factory/factory_test.sh ]; then
#       /tmp/sd/Factory/config.sh
#       exit
#   fi
# Patches /backup/init.sh to source /tmp/sd/lower_half_init.sh every boot, moves
# this Factory dir aside, then reboots. Also provisions WiFi from an optional
# Factory/configure_wifi.cfg before the reboot.

exec >/tmp/sd/unifi-install.log 2>&1
echo "=== yi-protect install $(date) ==="

if [ ! -f /tmp/sd/lower_half_init.sh ]; then
    echo "ERROR: /tmp/sd/lower_half_init.sh missing -- package incomplete"
    # Leave Factory/ in place so a corrected card retries on next boot.
    sync
    exit 1
fi

# Emergency flash backup before we touch the boot. Abort if it fails.
if [ -x /tmp/sd/unifi/script/backup-flash.sh ]; then
    /tmp/sd/unifi/script/backup-flash.sh || {
        echo "ERROR: flash backup failed; not patching boot"
        sync
        exit 1
    }
fi

# Optional WiFi credentials, dropped next to the installer as
# Factory/configure_wifi.cfg (wifi_ssid=/wifi_psk=). Written into the conf
# partition (mtd7) before the reboot, so the first post-install boot associates.
# Failure is non-fatal: the camera still installs and can be provisioned later
# with unifi/etc/configure_wifi.cfg (see init.sh).
if [ -f /tmp/sd/Factory/configure_wifi.cfg ] && [ -x /tmp/sd/unifi/script/configure-wifi.sh ]; then
    echo "provisioning wifi from Factory/configure_wifi.cfg"
    /tmp/sd/unifi/script/configure-wifi.sh /tmp/sd/Factory/configure_wifi.cfg || \
        echo "WARN: wifi provisioning failed; continuing install"
fi

if grep -q "/tmp/sd/lower_half_init.sh" /backup/init.sh 2>/dev/null; then
    echo "boot hook already present, skipping patch"
else
    echo "patching /backup/init.sh"
    sh /tmp/sd/unifi/install/install-backup-hook.sh
fi

# Rename ourselves away so the stock factory hook doesn't halt every boot.
if [ -e /tmp/sd/Factory ]; then
    mv /tmp/sd/Factory /tmp/sd/Factory.done
fi

sync
sync
sync
echo "=== done, rebooting ==="
reboot
