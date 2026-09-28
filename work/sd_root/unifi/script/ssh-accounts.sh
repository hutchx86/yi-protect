#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# (Re)build the SSH login accounts in the tmpfs /etc. Run by init.sh at boot
# and by unifi_avclient_go after the controller pushes a device credential.
#
#   root            unifi.cfg SSH_PASSWORD
#   Protect user    only with PROTECT_SSH=yes: the controller's user
#                   (etc/ssh_protect, "ubnt" until adoption), uid 0 like a real
#                   UniFi camera. SSH_PASSWORD, and also the controller's device
#                   password: etc/ssh_protect ("user:$6$...") is copied to
#                   /etc/ssh_protect, which our dropbear patch checks
#                   (work/dropbear/yp_extra_auth.c).
#
# An empty SSH_PASSWORD locks that password ("!"); there is no default and no
# blank login. PROTECT_SSH defaults to off: /etc/ssh_protect is then left
# empty, so a credential stored earlier on the card is not honoured.

UNIFI_PREFIX="${UNIFI_PREFIX:-/tmp/sd/unifi}"
CONF="$UNIFI_PREFIX/etc/unifi.cfg"
PROTECT="$UNIFI_PREFIX/etc/ssh_protect"
LIVE=/etc/ssh_protect

get_cfg() {
    grep -E "^$1=" "$CONF" 2>/dev/null | cut -d= -f2-
}

set_shadow() {
    # $1 = user, $2 = crypt hash or "!"; replace the entry, else append.
    if grep -q "^$1:" /etc/shadow; then
        sed -i "s|^$1:.*|$1:$2:1:0:99999:7:::|" /etc/shadow
    else
        printf '%s:%s:1:0:99999:7:::\n' "$1" "$2" >> /etc/shadow
    fi
}

HASH='!'
SSH_PASSWORD=$(get_cfg SSH_PASSWORD)
if [ -n "$SSH_PASSWORD" ] && [ -x "$UNIFI_PREFIX/bin/mkpasswd" ]; then
    h=$(printf '%s\n' "$SSH_PASSWORD" | "$UNIFI_PREFIX/bin/mkpasswd")
    [ -n "$h" ] && HASH="$h"
fi

sed -i 's|^root:[^:]*:|root:x:|' /etc/passwd
set_shadow root "$HASH"

# Drop accounts from an earlier run (tmpfs /etc resets only at boot): lines of
# exactly the shape written below (root excepted), plus any ubnt account.
for u in $(sed -n 's|^\([a-z_][a-z0-9_-]*\):x:0:0:\1:/root:/bin/ash$|\1|p' /etc/passwd); do
    [ "$u" = root ] || sed -i "/^$u:/d" /etc/passwd /etc/shadow
done
sed -i '/^ubnt:/d' /etc/passwd /etc/shadow
: > "$LIVE"
chmod 0600 "$LIVE"

case "$(get_cfg PROTECT_SSH)" in
    yes|true|1|on) ;;
    *) chmod 0600 /etc/shadow /etc/passwd 2>/dev/null; exit 0 ;;
esac

# The controller's username; validated the same way the client validates it
# before writing the file, since it lands in /etc/passwd.
PUSER=ubnt
LINE=""
if [ -f "$PROTECT" ]; then
    LINE=$(sed -n '1p' "$PROTECT")
    u=${LINE%%:*}
    case "$u" in
        ""|*[!a-z0-9_-]*|[!a-z_]*) LINE="" ;;
        *) PUSER="$u" ;;
    esac
fi
if [ "$PUSER" != root ]; then
    sed -i "/^$PUSER:/d" /etc/passwd
    echo "$PUSER:x:0:0:$PUSER:/root:/bin/ash" >> /etc/passwd
    set_shadow "$PUSER" "$HASH"
fi
[ -n "$LINE" ] && printf '%s\n' "$LINE" > "$LIVE"
chmod 0600 /etc/shadow /etc/passwd 2>/dev/null
exit 0
