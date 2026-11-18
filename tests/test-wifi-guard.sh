#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Host regression test for sd_root/yi-protect/script/wifi-guard.sh.
# Uses a scratch mtd7 image and a scratch SD tree, so it never touches a camera.
#
# Run: sh tests/test-wifi-guard.sh

set -u

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
guard="$root/sd_root/yi-protect/script/wifi-guard.sh"
init="$root/sd_root/yi-protect/script/init.sh"
conf="$root/sd_root/yi-protect/script/configure-wifi.sh"

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $*"; }
eq() { if [ "$1" = "$2" ]; then ok; else bad "$3 (got '$1', want '$2')"; fi; }
file() { if [ -f "$1" ]; then ok; else bad "missing file: $1"; fi; }
absent() { if [ ! -e "$1" ]; then ok; else bad "unexpected file: $1"; fi; }
out_has() { if grep -q "$1" "$T/out" 2>/dev/null; then ok; else bad "output missing: $1"; fi; }
out_hasnt() { if grep -q "$1" "$T/out" 2>/dev/null; then bad "output should not contain: $1"; else ok; fi; }

setup() {
    rm -rf "$T/yi-protect" "$T/mtd7" "$T/missing" "$T/log" "$T/out"
    mkdir -p "$T/yi-protect/etc" "$T/yi-protect/script"
    cp "$conf" "$T/yi-protect/script/configure-wifi.sh"
    chmod +x "$T/yi-protect/script/configure-wifi.sh"
    dd if=/dev/zero of="$T/mtd7" bs=1024 count=64 2>/dev/null
}

run() {
    : > "$T/out"
    WIFI_GUARD_YIP="$T/yi-protect" \
    WIFI_GUARD_MTD="${CASE_MTD:-$T/mtd7}" \
    WIFI_GUARD_REBOOT="echo REBOOT" \
    WIFI_GUARD_KILL=0 \
    WIFI_GUARD_LOG="$T/log" \
    sh "$guard" >"$T/out" 2>&1
    rc=$?
}

cfg() {
    # cfg SSID PSK FILENAME
    printf 'wifi_ssid="%s"\nwifi_psk="%s"\n' "$1" "$2" > "$T/yi-protect/etc/$3"
}

set_ssid() { printf '%s' "$1" | dd of="$T/mtd7" bs=1 seek=28 conv=notrunc 2>/dev/null; }
set_psk() { printf '%s' "$1" | dd of="$T/mtd7" bs=1 seek=92 conv=notrunc 2>/dev/null; }
ssid_of() { dd bs=1 skip=28 count=64 if="$T/mtd7" 2>/dev/null | tr -d '\000'; }

identity() {
    : > "$T/yi-protect/etc/yi_protect_client_go.crt"
    : > "$T/yi-protect/etc/yi_protect_client_go.key"
    : > "$T/yi-protect/etc/yi_protect_client_go.device-id"
    : > "$T/yi-protect/etc/yi_protect_client_go.adoption-uuid"
    printf 'true\n' > "$T/yi-protect/etc/yi_protect_client_go.adopted"
}

assert_cleared() {
    absent "$T/yi-protect/etc/yi_protect_client_go.crt"
    absent "$T/yi-protect/etc/yi_protect_client_go.key"
    absent "$T/yi-protect/etc/yi_protect_client_go.device-id"
    absent "$T/yi-protect/etc/yi_protect_client_go.adoption-uuid"
    eq "false" "$(cat "$T/yi-protect/etc/yi_protect_client_go.adopted" 2>/dev/null)" "adopted flag"
    file "$T/yi-protect/etc/yi_protect_client_go.awaiting-manage"
}

assert_kept() {
    file "$T/yi-protect/etc/yi_protect_client_go.crt"
    file "$T/yi-protect/etc/yi_protect_client_go.key"
    file "$T/yi-protect/etc/yi_protect_client_go.device-id"
    file "$T/yi-protect/etc/yi_protect_client_go.adoption-uuid"
    eq "true" "$(cat "$T/yi-protect/etc/yi_protect_client_go.adopted" 2>/dev/null)" "adopted flag"
}

sh -n "$guard" && ok || bad "sh -n wifi-guard.sh"
sh -n "$init" && ok || bad "sh -n init.sh"

# 1. Healthy boot: SSID present, adoption kept, no reboot.
setup
set_ssid TestNet; set_psk secret; identity
run
eq 0 "$rc" "healthy exit"
assert_kept
absent "$T/yi-protect/etc/yi_protect_client_go.awaiting-manage"
out_hasnt REBOOT

# 2. Wiped partition with a known-good config: restore and unadopt.
setup
cfg TestNet secret wifi.known-good; identity
run
eq 0 "$rc" "wipe exit"
assert_cleared
eq TestNet "$(ssid_of)" "restored SSID"
out_has REBOOT
file "$T/yi-protect/etc/configure_wifi.cfg.applied"

# 3. Wiped partition with no source: leave adoption alone.
setup
identity
run
eq 0 "$rc" "no-source exit"
assert_kept
out_hasnt REBOOT
out_has "no known-good"

# 4. Unreadable mtd7: not a wipe, never touch adoption.
setup
identity
CASE_MTD="$T/missing" run
eq 0 "$rc" "read-error exit"
assert_kept
out_hasnt REBOOT
out_has "cannot read"

# 5. First provision (pending cfg, no known-good): write, do not unadopt.
setup
cfg TestNet secret configure_wifi.cfg; identity
run
eq 0 "$rc" "first-provision exit"
eq TestNet "$(ssid_of)" "first-provision SSID"
out_has REBOOT
assert_kept
file "$T/yi-protect/etc/wifi.known-good"
file "$T/yi-protect/etc/configure_wifi.cfg.applied"

# 6. Pending cfg on a provisioned unit: change WiFi, keep adoption.
setup
set_ssid OldNet; set_psk old; identity
cfg NewNet new configure_wifi.cfg
run
eq 0 "$rc" "change exit"
eq NewNet "$(ssid_of)" "changed SSID"
out_has REBOOT
assert_kept
out_hasnt "adoption identity cleared"

# 7. Legacy .applied as the only source: still restore and unadopt.
setup
cfg TestNet secret configure_wifi.cfg.applied; identity
run
eq 0 "$rc" "legacy wipe exit"
assert_cleared
eq TestNet "$(ssid_of)" "legacy restored SSID"
out_has REBOOT

# 8. Write failure: configure-wifi exits 0 without writing, guard must not
#    unadopt or reboot.
setup
cfg TestNet secret wifi.known-good; identity
chmod 0444 "$T/mtd7"
run
chmod 0644 "$T/mtd7"
eq 0 "$rc" "write-fail exit"
assert_kept
out_hasnt REBOOT
out_has "did not take"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
