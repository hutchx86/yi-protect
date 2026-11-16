# AGENTS.md

Guidance for coding agents working in this repository. Read `README.md` first for what
the project is; this file covers how to change it.

## Build and verify

    make -C flv_bridge test      # host parser self-test
    ./build_sd.sh                # full SD-card build (all binaries)

Run the real command before claiming a change works, and report the exact command and
result. On-camera behaviour is the final check: a change is not done until the camera
shows it. Record the deployed binary hashes and the evidence in the project status.

## Layout

- `flv_bridge/`: fshare ring parser, FLV muxer and the four output channels.
- `goclient/`: the Protect-side client (discovery, manage API, WSS, stream routing).
- `sd_root/`: the SD-card layout installed on the camera.
- `script/`: init, watchdog, mediad and per-model bring-up scripts.
- `docs/`: protocol and implementation notes.
- `build/`: generated; do not edit.

## Code

- Smallest correct change. Match the surrounding style; leave unrelated lines alone.
- Comments explain why, not what. No commented-out code or banner art.
- Handle errors at boundaries and fail loudly. Never swallow an error.
- Add a regression test for every fix.

## Boundaries

- Never commit firmware images, flash dumps, captured credentials, device
  certificates or private keys. Identity material belongs on the SD card at runtime.
- Per-model facts belong in `sd_root/yi-protect/etc/model_table`, never in a code
  branch on the model name.
- Do not push, force-push or rewrite published history unless the maintainer asks.

## AI assistance

AI-assisted changes are allowed. The author is responsible for reviewing and testing
them. Disclose material AI assistance; do not add generated-by trailers.
