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
  (the same linear curve real hardware uses). Dragging the slider to its minimum
  (**1%**) intentionally mutes the mic: the capture gain drops to zero and the
  bridge substitutes silent AAC/Opus frames, so the audio track **keeps flowing**
  (silenced) rather than the camera being told to stop sending audio.
- **Snapshots**, **motion events**, **PTZ** (models with a motorized base), and
  rudimentary **IR / night-vision** control.
- **Two-way audio (talkback)** — from the Protect mobile/web app to the speaker.
- **SSH access** — a Dropbear server for `root` (password from `yi-protect.cfg`
  `SSH_PASSWORD`). Protect's own account (`ui`; `ubnt` before adoption) is
  accepted only with `PROTECT_SSH=yes`. See [Access](#access-ssh).
- **Settings page** — a small HTTP page on `:80` (`yi-protect.cfg` `WEBUI_PORT`,
  `0` = off) for pinning the optional mediad's image settings. It is
  **unauthenticated**: anyone on the LAN can change them.
- **Optional Yi cloud** — the `yi-protect.cfg` key `YI_CLOUD` also runs the stock Yi
  cloud daemons (`cloud`, `p2p_tnp`, `oss`) so the camera still appears in the
  YI app alongside Protect. It is **on by default**, except when the `mediad`
  encoder is active (`IS_MEDIAD=auto`/`yes` with mediad installed) where it
  defaults off; set `yes`/`no` to force it. Off =
  local-only, no Yi cloud traffic.
- **Selectable encoder** — yi-protect runs either the stock Yi `rmm` encoder or
  the clean-room [yi-mediad](https://github.com/hutchx86/yi-mediad) daemon.
  `yi-protect.cfg IS_MEDIAD=yes` runs `mediad` (H.264 + H.265, advanced picture
  controls); `no` keeps stock `rmm` (H.264 only). The two never run together.
  H.265 "super encoding" is advertised to the controller **only** on the
  `mediad` path, so Protect cannot pick a codec the active encoder cannot
  produce. The published release image ships the stock `rmm` path only; with
  `IS_MEDIAD=auto` (the default) a card that has `mediad` installed runs it
  automatically — see [Optional mediad encoder](#optional-mediad-encoder).
- **Self-contained SD deploy** — everything in the image builds from source into
  `/tmp/sd/yi-protect/`; no yi-hack install required at runtime.
- **One image, any supported camera** — the model is auto-detected at boot, so
  the same SD card works across models with no per-camera edits.
- **Safety net** — first boot dumps the camera's full raw flash to the SD card
  for emergency recovery, before the boot is modified.

## Requirements

- A supported Yi camera (see below) with an SD card, and root access to modify
  its boot.
- A build host with Go (1.23+), `cmake`, `wget`, `patch`, `make`, and the
  cross-toolchain (cloned manually, below).
- A UniFi Protect controller on the same network.

## Supported hardware

Allwinner **sun8iw19** (`sun8iw19p1`), ~60 MB RAM, BusyBox userland. The SoC on
these cameras is a **QG2101A/B** (齐感科技 / SensLab) — a rebadged Allwinner
**V831**-class part: single Cortex-A7 (800 MHz-class), 0.2 TOPS NPU, 64 MB DDR2
in-package, QFN88, 2-lane MIPI CSI. ("V831/V833" is Allwinner's own name for the
family; project notes use the two loosely.) Developed against:

- Yi **Pro 2k** (`y623`; PCB silkscreen may read `y621`)
- Yi **Dome Camera U** (2K) (`h51ga`)
- Yi **Dome Camera U** (Full HD) (`h52ga`)
- Yi **Dome Guard** (`r35gb`)
- Yi **1080p Home** (`y291ga`)

The Go client spoofs a **UVC G3 Instant** (`SAV532Q`) by default, the closest
native product to these sensors.

## How it works

The camera keeps its stock vendor firmware and kernel; this project adds a
self-contained stack on the SD card (`/tmp/sd/yi-protect/`). A patched
`/backup/init.sh` sources our `lower_half_init.sh`. On every boot it builds the
bring-up script from the camera's own stock `lower_half_init.sh`
(`yi-protect/script/gen-lower-half.sh`): the stock hardware bring-up runs unchanged,
and the vendor app launch is replaced by ours, which hands off to
`yi-protect/script/init.sh`. That brings up our binaries and leaves the stock
low-level daemons (sensor/ISP/encoder) in place. If the stock script is not in a
shape the generator recognises, it runs unmodified (the camera boots without our
app) and `/tmp/lower_half.log` says why.

| Component | Language | Role |
| --- | --- | --- |
| `yi_protect_flv_bridge` | C++ | Reads the stock encoder's shared-memory ring (`fshare`) directly, muxes UniFi's `extendedFlv` (video, native AAC, and a transcoded Opus track) and pushes it over a self-dialled TCP socket. No RTSP, no LIVE555. |
| `yi_protect_avclient_go` | Go | Control plane: L2/UDP discovery, WSS adoption/control, clock sync, snapshots (`yi_protect_snapshot`), motion events, PTZ, and the camera-side manage API on `:443`. |
| `yi_protect_snapshot` | C++ | Snapshot tool for Protect's `GetRequest`: reads the ring's next keyframe, decodes it (FFmpeg) and writes a JPEG (libjpeg). Replaces yi-hack's `imggrabber`. |
| `talkback_rx` | C | Talkback receiver: decodes ADTS AAC / RTP Opus from UDP `:7004` to 16 kHz PCM and drives the speaker + amp. |
| `cpld_ctl` | C | CPLD / IR-LED / amp control helper for the `/dev/cpld_periph` ioctls. |
| `mixer_set` | C | Sets the codec capture gain (ALSA) from Protect's Microphone Level. |
| `mkpasswd` | C | MD5-crypt helper that hashes `SSH_PASSWORD` at boot. |
| `downloader` | C | Static HTTPS client (Mbed TLS); used only to fetch the vendor libs for a vendor-linked mediad. |
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

The card root should now contain `lower_half_init.sh`, `Factory/` and `yi-protect/`.

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

The package carries **no per-camera identity**: device-id, TLS cert/key, adopt
state, dropbear host keys and the model token are generated on the camera at
boot. `build_sd.sh` refuses to package symlinks (FAT32) or any such identity
left in the tree, so one image is safe for every unit.

To update later, overwrite the card contents with a newer package and reboot.

## WiFi provisioning

The stock camera has no AP-mode provisioning of its own (the Yi app does that),
so yi-protect sets WiFi from a credentials file on the card:

> **Untested on hardware.** The conf-partition write is unit-tested against a
> synthetic mtd7 (offsets, padding, idempotency, validation), but a real
> first-boot association has not been validated yet — WiFi provisioning still
> needs an on-camera test.

- **First boot (fresh card):** rename `Factory/configure_wifi.cfg.ori` to
  `Factory/configure_wifi.cfg`, edit `wifi_ssid=` / `wifi_psk=`, then power on.
  The installer writes the credentials, then reboots once.
- **Later:** rename `yi-protect/etc/configure_wifi.cfg.example` to
  `configure_wifi.cfg`, edit it, drop it on the card, and reboot. `init.sh`
  applies it, reboots once, and renames it `.applied`.

Format is plain `KEY=value` (spaces allowed, no backslash, max 63 chars). The
value may be bare or wrapped in one pair of matching double/single quotes; the
quotes are stripped before writing, and are recommended so special characters and
leading/trailing spaces are unambiguous. See the example file.
`yi-protect/script/configure-wifi.sh` writes the SSID at
offset 28 and the PSK at offset 92 of the conf partition (`/dev/mtdblock7`),
after backing it up to the card. That is the same partition the stock WiFi stack
reads, so it works without the Yi app or cloud.

An on-camera **hotspot / captive-portal flow is not yet possible**: the Yi
firmware ships no `hostapd`, no `udhcpd`, and a `wpa_supplicant` built without AP
mode. It needs a cross-built AP daemon.

## Configuration

`yi-protect/etc/yi-protect.cfg` is the one file an admin edits (plain
`KEY=value`); per-camera runtime state is kept separately in the
`yi_protect_client_go.*` files beside it.

| Key | Values | Meaning |
| --- | --- | --- |
| `RESOLUTION` | `high`/`low`/`both` | Which bridge video channels to publish. |
| `AUDIO` | `aac`/`no` | Publish the mic audio track. |
| `PTZ` | `yes`/`no`/empty | Declare the PTZ feature; empty = the model table's `ptz` column. |
| `IS_MEDIAD` | `yes`/`no`/`auto` | Encoder path. `auto` (default) runs `mediad` when installed, else `rmm`; `yes`/`no` force it. |
| `MEDIAD_3DNR` | `yes`/`no` | Let Protect's `enable3dnr` drive mediad's temporal denoise (`tdf`); `no` = `tdf` follows `mediad.conf`. |
| `YI_CLOUD` | `yes`/`no`/empty | Run the stock Yi cloud daemons; empty = on unless the `mediad` encoder is active. |
| `SSH_PASSWORD` | text | Root SSH password; hashed at boot, never stored in `/etc`. Empty = locked (no password login). |
| `PROTECT_SSH` | `yes`/`no` | Also accept Protect's device password (as `ui`, or `ubnt` before adoption) with root rights. |
| `WATCHDOG_INTERVAL` | seconds | Supervisor poll interval. |

Optional overrides (commented in the file): `CONTROLLER` (`host[:port]`, overrides
DHCP discovery), `MODEL`/`SYSID`/`FWVERSION` (spoofed identity; default "UVC G3
Instant"), `STATIC_IP`/`STATIC_MASK`/`STATIC_GW`/`STATIC_DNS1`/`STATIC_DNS2`
(static network), and `WEBUI_PORT` (mediad settings page on `:80`; `0` = off).

To set WiFi credentials, see [WiFi provisioning](#wifi-provisioning).

## Optional mediad encoder

`mediad` is a separate clean-room daemon (repo:
[hutchx86/yi-mediad](https://github.com/hutchx86/yi-mediad)) that replaces the
stock Yi `rmm` encoder while publishing the same fshare ring the bridge reads.
It adds H.265/HEVC, per-model capture geometry, and advanced picture controls,
and is the only encoder path on which H.265 is advertised.

The published release image is **rmm-only**. To add `mediad`:

- **Existing yi-protect install** — overlay the yi-mediad package onto the card;
  its installer copies the files, reroutes `init.sh`/`watchdog.sh`, and keeps
  `.pre-mediad` backups.
- **Fresh camera** — install yi-protect first, then overlay; or build a
  `mediad`-bundled image from source (build the yi-mediad package first so
  `build_sd.sh` picks up its `dist/`).

`IS_MEDIAD` defaults to `auto`: the boot scripts run `mediad` whenever it is
fully installed (`bin/mediad` + `script/mediad.sh` + `lib/libvenc_base.so`) and
fall back to stock `rmm` otherwise, so an overlay needs no config edit. Set
`IS_MEDIAD=no` to force `rmm`, or `yes` to require `mediad`.

A prebuilt overlay archive is published on the yi-mediad
[Releases](https://github.com/hutchx86/yi-mediad/releases) page —
`mediad-overlay-<rev>.tar.gz`, built in CI from the clean-room sources.

Full instructions for both starting points are in the yi-mediad repo:
[`package/README.md`](https://github.com/hutchx86/yi-mediad/blob/main/package/README.md).

## Build from source

The build needs the pinned `yi-hack-Allwinner-v2` tree and a ~1.2 GB
cross-toolchain. Neither is vendored: `build_sd.sh` clones and pins the yi-hack
tree itself, so only the toolchain must be cloned into the project's shared
`repos/` directory (a sibling of this repo; override the location with
`REPOS_DIR`):

```
mkdir -p ../repos
git clone https://github.com/lindenis-org/lindenis-v536-prebuilt \
  ../repos/lindenis-v536-prebuilt
```

`build_sd.sh` auto-detects the toolchain clone (`lindenis-v536-prebuilt` or
`lindenis-v833-prebuilt`, or `toolchain-sunxi-musl`); override with
`TOOLCHAIN_DIR`.

Then:

```
# Host-only self-test of the FLV/fshare parser (no cross toolchain needed)
make -C flv_bridge test

# Full SD package: build_sd.sh builds every binary from source (including the
# static downloader with Mbed TLS) into sd_root/ and writes
# yi-protect-<rev>.tar.gz
./build_sd.sh

# Release: runs build_sd.sh, then writes yi-protect-<rev>-src.tar.gz
# (corresponding source) and SHA256SUMS
./release.sh
```

Config lives in `sd_root/yi-protect/etc/yi-protect.cfg` (resolution, audio, optional
static controller override, PTZ auto-detect, optional Yi cloud).

Per-model **facts** live in `sd_root/yi-protect/etc/model_table` — one row per
camera (sensor, fshare ring geometry, HIGH-channel resolution, PTZ, an optional
pinned HIGH bitrate). The bridge,
the client, the boot scripts, `detect-model.sh` and `build_sd.sh` all read that
one table and never hardcode a model name, so adding a camera is a single row.
Hardware bring-up needs no per-model file: it comes from each camera's own
stock script at boot. An unlisted model gets a conservative default and a
warning rather than a silent guess.

## Access (SSH)

A single Dropbear SSH server starts on boot (`:22`), with per-camera host keys
generated on first boot. There is **no default password** and no blank-password
login.

| user | password(s) | when |
|---|---|---|
| `root` | `yi-protect.cfg` `SSH_PASSWORD` | always (locked if empty) |
| Protect's user (`ui` on current Protect; `ubnt` before adoption) | `SSH_PASSWORD` **or** Protect's device password | only with `PROTECT_SSH=yes`; root-equivalent (uid 0), like a native UniFi camera |

```
ssh root@<camera-ip>        # your SSH_PASSWORD
ssh ui@<camera-ip>          # PROTECT_SSH=yes: Protect's device password or SSH_PASSWORD
```

- **Set `SSH_PASSWORD` in `yi-protect/etc/yi-protect.cfg` before the first boot.** Empty
  means no password login for root.
- `PROTECT_SSH` defaults to `no`: Protect's credential push is acknowledged and
  nothing of it is stored or accepted. With `yes`, Protect pushes its device
  credential (the NVR-wide device password, shown in Protect's settings) on
  every connect; the camera stores only its SHA-512 hash (`yi-protect/etc/ssh_protect`),
  and a password change in Protect takes effect on the next connect. Switching
  it back to `no` (and rebooting) stops a stored credential being accepted.
- `SSH_PASSWORD` is hashed at boot (MD5-crypt, the scheme the camera's libc
  checks). Protect's SHA-512 hash is checked by a small patch to our Dropbear
  build (`dropbear/`), since the camera's libc cannot verify it.
- To change either setting, edit `yi-protect.cfg` on the card and reboot. The card
  is FAT, so anyone holding it can read `yi-protect.cfg`; treat the card as a secret.

## Backup & recovery

The first boot dumps **every raw MTD partition** (uboot, boot, rootfs, home,
backup, env, mfg, conf) to `/tmp/sd/yi-protect-backup/<model>/`, together with
`md5sums`, `mtd.txt`, and `RESTORE.md` + `restore.sh`. Keep a copy off the
camera. Verify with `md5sum -c md5sums`.

`restore.sh` is a last-resort helper that rewrites a single partition (needs
`flash_erase` + `nandwrite`, refuses mounted partitions, asks for typed
confirmation). The safest recovery for a bricked camera is U-Boot/UART or the
stock vendor flash tool.

## Troubleshooting

**No camera audio in Protect (live view *or* recordings).** Check the camera's
**Microphone Level** slider before anything else. Its **minimum position (1%) is
a deliberate mute, not a quiet setting** — Protect's slider floors at 1 and never
sends 0, so 1% is mapped to "muted": the codec capture gain is driven to zero and
the bridge substitutes silent AAC/Opus frames (the audio track keeps flowing, just
silenced). Move the slider up and audio returns. This is easy to mistake for a
broken microphone — e.g. a camera left at 1% after a silence test looks dead. See
the **Audio** note under [Features](#features).

## Credits & special thanks

**A very special thank you to [roleoroleo](https://github.com/roleoroleo).** This
project would not exist without his
[**yi-hack-Allwinner-v2**](https://github.com/roleoroleo/yi-hack-Allwinner-v2):
the SD-card boot hook, the per-model hardware bring-up scripts, and the helper
binaries (`ipc_cmd`, `dropbear`, the patched `alsa-lib`) are all
built from his tree. The `fshare` shared-memory framing was reverse-engineered
with reference to his **rRTSPServer**, and the on-device boot flow follows the
yi-hack pattern. Thank you.

**A special thank you to [dciancu](https://github.com/dciancu).** His
[**unifi-protect-unvr-docker-arm64**](https://github.com/dciancu/unifi-protect-unvr-docker-arm64)
was the original inspiration and motivation for this project — it demonstrated
running UniFi Protect off Ubiquiti hardware — and his published findings
informed the early reverse engineering. Thank you.

Further thanks: [unifi-cam-proxy](https://github.com/keshavdv/unifi-cam-proxy)
(control-protocol reference), [FAAD2](https://github.com/knik0/faad2) (AAC),
[libopus](https://opus-codec.org/),
[gorilla/websocket](https://github.com/gorilla/websocket), and the
[lindenis / Allwinner SDK](https://github.com/lindenis-org) toolchain. Full
attribution is in [`NOTICE`](NOTICE).

## Legal

- **Not affiliated with, or endorsed by, Ubiquiti Inc.** "UniFi" and "UniFi
  Protect" are trademarks of Ubiquiti Inc.
- **Not affiliated with, or endorsed by, YI Technology or Kami Vision**, the
  makers of YI cameras. "YI" and "Kami" are trademarks of their respective
  owners.
- **No Ubiquiti firmware or binaries are distributed here.** Vendor firmware,
  extracted root filesystems and stock scripts/watermark bitmaps are not in this
  repository; supply your own. This repo is our own source, scripts and
  documentation, plus a few helper scripts vendored from
  [yi-hack-Allwinner-v2](https://github.com/roleoroleo/yi-hack-Allwinner-v2)
  (`yi-protect/script/ethdhcp.sh`, `wifidhcp.sh`); other third-party code is
  fetched from upstream at build time. See [`NOTICE`](NOTICE).
- **This project exists to ensure interoperability between UniFi Protect and
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
(`yi-protect/script/ethdhcp.sh`, `wifidhcp.sh`) are modified from the MIT-licensed
yi-hack-Allwinner-v2 scripts of the same name and are combined with our code under
AGPL-3.0-or-later (upstream MIT notice:
`LICENSES/yi-hack-Allwinner-v2-MIT.txt`). The compiled SD image additionally
bundles GPL/LGPL components (yi-hack GPL-3.0 helpers, FAAD2 GPL-2.0-or-later,
LGPL-2.1 libasound, and FFmpeg/libjpeg-turbo statically linked into
`yi_protect_snapshot`); the release tarball includes `LICENSE`, `NOTICE` and
`SOURCES.txt` with the corresponding source locations and a written offer. See
[`NOTICE`](NOTICE).
