# yi-protect (Yi camera → UniFi Protect bridge)

A bridge that connects cheap **Yi cameras running on the Allwinner platform** —
specifically the models supported by
[**yi-hack-Allwinner-v2**](https://github.com/roleoroleo/yi-hack-Allwinner-v2) —
to **UniFi Protect**, presenting them as if they were native cameras.

The camera keeps its stock firmware and does the encoding; the bridge muxes its
output into Protect's native `extendedFlv` ingest, so the **Protect console does
no decode/re-encode work** for these streams (handy on a small console). It adds
low-latency live view, talkback, and rudimentary IR control on top.

This is a reverse-engineering / interoperability **proof of concept**. It is
**not affiliated with, or endorsed by, Ubiquiti Inc.**, and it will **not** give
you as seamless an experience as a genuine Ubiquiti camera — expect rough edges
and the occasional missing feature. It is, however, a great way to put old Yi
cameras lying around back to work.

> **⚠ Proof of concept, not a product.** It runs on my own hardware.
> Behaviour already differs from real UniFi cameras in places and parity is not
> guaranteed.

> **Not affiliated with Ubiquiti Inc.** "UniFi" and "UniFi Protect" are
> trademarks of their respective owners, used here descriptively to indicate
> interoperability. No Ubiquiti firmware or binaries are distributed here. See
> [Legal](#legal).

> **AI-assisted development.** Large parts of this project were produced with
> LLMs (Claude Code and DeepSeek), always under strict human supervision,
> review, and real-hardware testing. Verify anything you rely on.

## Status

Working and tested on real hardware: adoption, live video, recordings,
snapshots, motion, talkback and PTZ all function against a real Protect
controller. It is still a moving target — expect rough edges.

## Features

- **Discovery & adoption** — UBNT L2/UDP discovery and the WSS control protocol;
  the controller adopts it like real hardware.
- **Live video & recording** — the stock encoder's output is muxed into UniFi's
  `extendedFlv` and pushed over a plain TCP socket. Low latency; no RTSP hop.
- **Audio** — the camera's native AAC plus a transcoded Opus track for web live.
  The mic volume follows Protect's "Microphone Level" via the codec capture gain
  (the same linear curve real hardware uses); the slider minimum silences it.
- **Snapshots**, **motion events**, **PTZ** (models with a motorized base), and
  rudimentary **IR / night-vision** control.
- **Two-way audio (talkback)** — from the Protect mobile/web app to the speaker.
- **Self-contained SD deploy** — everything builds from source into
  `/tmp/sd/unifi/`; no yi-hack install required at runtime.
- **One image, any supported camera** — the model is auto-detected at boot, so
  the same SD card works across models with no per-camera edits.
- **Safety net** — first boot dumps the camera's full raw flash to the SD card
  for emergency recovery, before the boot is modified.

## Requirements

- A supported Yi camera (see below) with an SD card, and root access to modify
  its boot.
- A build host with Go (1.23+), `cmake`, `wget`, and the cross-toolchain
  (cloned manually, below).
- A UniFi Protect controller on the same network.

## Supported hardware

Allwinner **sun8iw19**, ~60 MB RAM, BusyBox userland. Developed against:

- Yi **Pro 2k** (`y623`; PCB silkscreen may read `y621`)
- Yi **Dome Camera U** (Full HD) (`h52ga`)
- Yi **Dome Guard** (`r35gb`)

The Go client spoofs a **UVC G3 Instant** (`SAV532Q`) by default, the closest
native product to these sensors.

## How it works

The camera keeps its stock vendor firmware and kernel; this project adds a
self-contained stack on the SD card (`/tmp/sd/unifi/`). A patched
`/backup/init.sh` sources our `lower_half_init.sh`, which hands off to
`unifi/script/init.sh`. That brings up our binaries and leaves the stock
low-level daemons (sensor/ISP/encoder) in place.

| Component | Language | Role |
| --- | --- | --- |
| `unifi_flv_bridge` | C++ | Reads the stock encoder's shared-memory ring (`fshare`) directly, muxes UniFi's `extendedFlv` (video, native AAC, and a transcoded Opus track) and pushes it over a self-dialled TCP socket. No RTSP, no LIVE555. |
| `unifi_avclient_go` | Go | Control plane: L2/UDP discovery, WSS adoption/control, clock sync, snapshots (`imggrabber`), motion events, PTZ, and the camera-side manage API on `:443`. |
| `talkback_rx` | C | Talkback receiver: decodes ADTS AAC / RTP Opus from UDP `:7004` to 16 kHz PCM and drives the speaker + amp. |
| `cpld_ctl` | C | CPLD / IR-LED / amp control helper for the `/dev/cpld_periph` ioctls. |
| boot scripts | shell | `init.sh`, `watchdog.sh`, network/identity detection, model auto-detection. |

Protocol findings (wire formats, message shapes, discovery TLVs, the
`extendedFlv` trailer clock, talkback framing) come from reverse engineering
done for interoperability and are not published in this repository.

## Install (prebuilt)

### 1. Download and verify

Get `yi-protect-<rev>.tar.gz` and `SHA256SUMS` from
[Releases](https://github.com/hutchx86/yi-protect/releases), then verify:

```
sha256sum -c SHA256SUMS
```

### 2. Write it to an SD card

The stock camera only reads **FAT32**; one partition using the whole card is
fine, and the package extracts to the **root**.

```
# Replace /dev/sdX1 with your card's partition (this ERASES the card)
sudo umount /dev/sdX1 2>/dev/null
sudo mkfs.vfat -F32 /dev/sdX1
sudo mount /dev/sdX1 /mnt

tar xzf yi-protect-<rev>.tar.gz -C /mnt
sync && sudo umount /mnt
```

The card root should now contain `lower_half_init.sh`, `Factory/` and `unifi/`.

### 3. First boot (install)

Insert the card and power on. The stock firmware runs the Factory hook, which:

1. writes a **full raw-flash backup** to the card
   (`/tmp/sd/yi-protect-backup/<model>/`) — before anything is modified,
2. patches `/backup/init.sh` to boot yi-protect from the SD, and
3. reboots.

### 4. Adopt

On the post-install boot the camera comes up in Protect as a new, unadopted
device — adopt it like any UniFi camera. The model is auto-detected, so the same
card works in any supported camera. SSH is at `root@<camera-ip>`
(see [Access](#access-ssh)).

To update later, overwrite the card contents with a newer package and reboot.

## WiFi provisioning

The stock camera has no AP-mode provisioning of its own (the Yi app does that),
so yi-protect sets WiFi from a credentials file on the card:

- **First boot (fresh card):** rename `Factory/configure_wifi.cfg.ori` to
  `Factory/configure_wifi.cfg`, edit `wifi_ssid=` / `wifi_psk=`, then power on.
  The installer writes the credentials, then reboots once.
- **Later:** rename `unifi/etc/configure_wifi.cfg.example` to
  `configure_wifi.cfg`, edit it, drop it on the card, and reboot. `init.sh`
  applies it, reboots once, and renames it `.applied`.

Format is plain `KEY=value` (spaces allowed, no quotes, no backslash, max 63
chars); see the example file. `unifi/script/configure-wifi.sh` writes the SSID at
offset 28 and the PSK at offset 92 of the conf partition (`/dev/mtdblock7`),
after backing it up to the card. That is the same partition the stock WiFi stack
reads, so it works without the Yi app or cloud.

An on-camera **hotspot / captive-portal flow is not yet possible**: the Yi
firmware ships no `hostapd`, no `udhcpd`, and a `wpa_supplicant` built without AP
mode. It needs a cross-built AP daemon (see `todo.md`).

## Build from source

The `repos/yi-hack-Allwinner-v2` submodule and the ~1.2 GB cross-toolchain are
needed. The toolchain is **not** vendored — clone it to the expected path first:

```
git submodule update --init --recursive

git clone https://github.com/lindenis-org/lindenis-v536-prebuilt \
  repos/lindenis-v536-prebuilt
```

Then:

```
# Host-only self-test of the FLV/fshare parser (no cross toolchain needed)
make -C work/flv_bridge test

# Full SD package: builds every binary from source into work/sd_root/ and
# writes work/yi-protect-<rev>.tar.gz (+ SHA256SUMS)
work/release.sh
```

Config lives in `work/sd_root/unifi/etc/unifi.cfg` (resolution, audio, optional
static controller override, PTZ auto-detect).

## Access (SSH)

A single Dropbear SSH server starts on boot (`:22`), with per-camera host keys
generated on first boot. It serves two independent accounts:

| user | password | purpose |
|---|---|---|
| `root` | `unifi.cfg` `SSH_PASSWORD` (default `admin`) | human/admin login |
| `ubnt` | `unifi.cfg` `SSH_UBUNT_PASSWORD` (default `ubnt`) | account native Protect cameras expose; managed by the controller |

```
ssh root@<camera-ip>        # admin
ssh ubnt@<camera-ip>        # Protect
```

At boot `init.sh` hashes each password to the one scheme the camera's libc
supports (MD5-crypt, `$1$`) and installs it in `/etc/shadow`, replacing the stock
blank root password. Edit `SSH_PASSWORD` / `SSH_UBUNT_PASSWORD` in
`unifi/etc/unifi.cfg` on the card and reboot to change them; an empty
`SSH_PASSWORD` keeps the stock blank root login. Keeping `ubnt`'s credential
separate from root's means a controller credential rotation cannot lock out the
root login (the controller push is not applied yet -- `todo.md` item 26).

## Backup & recovery

The first boot dumps **every raw MTD partition** (uboot, boot, rootfs, home,
backup, env, mfg, conf) to `/tmp/sd/yi-protect-backup/<model>/`, together with
`md5sums`, `mtd.txt`, and `RESTORE.md` + `restore.sh`. Keep a copy off the
camera. Verify with `md5sum -c md5sums`.

`restore.sh` is a last-resort helper that rewrites a single partition (needs
`flash_erase` + `nandwrite`, refuses mounted partitions, asks for typed
confirmation). The safest recovery for a bricked camera is U-Boot/UART or the
stock vendor flash tool.

## Credits & special thanks

**A very special thank you to [roleoroleo](https://github.com/roleoroleo).** This
project would not exist without his
[**yi-hack-Allwinner-v2**](https://github.com/roleoroleo/yi-hack-Allwinner-v2):
the SD-card boot hook, the per-model hardware bring-up scripts, and the helper
binaries (`ipc_cmd`, `dropbear`, `imggrabber`, the patched `alsa-lib`) are all
built from his tree. The `fshare` shared-memory framing was reverse-engineered
with reference to his **rRTSPServer**, and the on-device boot flow follows the
yi-hack pattern. Thank you.

Further thanks: [unifi-cam-proxy](https://github.com/keshavdv/unifi-cam-proxy)
(control-protocol reference), [FAAD2](https://github.com/knik0/faad2) (AAC),
[libopus](https://opus-codec.org/),
[gorilla/websocket](https://github.com/gorilla/websocket), and the
[lindenis / Allwinner SDK](https://github.com/lindenis-org) toolchain. Full
attribution is in [`NOTICE`](NOTICE).

## Legal

- **Not affiliated with, or endorsed by, Ubiquiti Inc.** "UniFi" and "UniFi
  Protect" are trademarks of Ubiquiti Inc.
- **No Ubiquiti firmware or binaries are distributed here.** Vendor firmware,
  extracted root filesystems and stock scripts/watermark bitmaps are not in this
  repository; supply your own. This repo is our own source, scripts and
  documentation, plus a few helper scripts vendored from
  [yi-hack-Allwinner-v2](https://github.com/roleoroleo/yi-hack-Allwinner-v2)
  (`unifi/script/ethdhcp.sh`, `wifidhcp.sh`) and the BSD-licensed libopus
  headers; see [`NOTICE`](NOTICE).
- **This project exists to ensure interoperability between Unifi Protect and
  other cameras.** This interoperability goal is recognised under EU law:
  Directive 2009/24/EC (the Software Directive), **Art. 6**, which permits
  decompilation and reverse engineering to achieve interoperability with an
  independently created program without the rightsholder's authorisation, and
  **Art. 5(3)**, which permits observing, studying or testing the functioning of
  the program to determine its underlying ideas and principles; Regulation (EU)
  2024/903 (the Interoperable Europe Act); and Regulation (EU) 2022/1925 (the
  Digital Markets Act), Art. 6(7), which imposes interoperability obligations on
  gatekeepers.
- Intended for interoperability and personal use on hardware you own. Reverse
  engineering may be restricted in your jurisdiction; you are responsible for
  how you use it.

## Disclaimer

**This is a proof-of-concept project, not a production-ready system.** It is not
recommended for an actual production deployment. Large parts were produced with
LLMs (Claude Code and DeepSeek) under strict human supervision and real-hardware
testing — review and verify everything yourself. **This software is provided "as
is", without warranty of any kind.** It modifies your camera's boot process and
runs custom binaries on it: you can **brick the camera**, lose recordings, and
void its warranty. Recovery may require physical access and is never guaranteed.
By using this project you accept full responsibility for any damage, data loss,
downtime, or other consequences. **The authors and contributors are not
responsible or liable for any loss or damage arising from its use.**

Only proceed if you understand the risks and are working on hardware you own.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). This program can be run as a network
service, so section 13 of the AGPL requires anyone running a modified version
to offer its corresponding source to users interacting with it over a network.
The project's own code is AGPL-3.0-or-later; the two vendored yi-hack scripts
(`unifi/script/ethdhcp.sh`, `wifidhcp.sh`) remain GPL-3.0-or-later and are
combined with it under GPLv3/AGPLv3 section 13. The compiled SD image bundles
GPL/LGPL components (yi-hack GPL-3.0 helpers, FAAD2 GPL-2.0-or-later, LGPL-2.1
libasound, and FFmpeg/libjpeg-turbo statically linked into `imggrabber`); the
release tarball includes `LICENSE`, `NOTICE` and `SOURCES.txt` with the
corresponding source locations and a written offer. See [`NOTICE`](NOTICE).
