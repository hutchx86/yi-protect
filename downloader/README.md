# downloader

A small, fully static HTTP/HTTPS file fetcher for the camera, which has no
HTTPS client and no CA store. `init.sh` (libfetch) uses it to fetch the
optional vendor H.264 libraries and checks their md5 afterwards.

```
downloader [--insecure|-k|--no-check-certificate] [--ca FILE] <url> <outfile>
```

- Follows redirects, handles chunked transfer encoding, and resumes a partial
  `<outfile>` with an HTTP Range request.
- Exits non-zero when the response is truncated or the connection aborts, so a
  short file is never reported as a success.

## Certificates

By default it verifies the server certificate (chain and host name) against a
CA bundle, searched in this order: `--ca FILE`, `$DOWNLOADER_CA`,
`$SSL_CERT_FILE`, `$CURL_CA_BUNDLE`, then the usual system paths. With no
bundle and no `-k` it refuses to connect.

`-k` skips authenticating the server (the connection is still TLS). It is the
mode used on the camera; the md5 check on every fetched file is what makes that
acceptable. To verify instead, put a CA bundle on the SD card and pass
`--ca /path/to/bundle`.

## Build

`build_sd.sh` runs `build.sh` with its toolchain. Standalone:

```
TCBIN=/path/to/toolchain-sunxi-musl/gcc/linux-x86/arm/toolchain-sunxi-musl/toolchain/bin sh build.sh
```

`build.sh` fetches Mbed TLS 2.28.8 from upstream, checks its sha256, builds its
static libraries and links `downloader` against them (`build/` and the binary
are not in git). The build is reproducible: the same toolchain gives the same
binary.

## Test

`test/serve.py` is a local HTTP/HTTPS server with fixed-size, truncated,
reset, chunked and Range-capable endpoints for regression tests:

```
python3 test/serve.py 18080 http
python3 test/serve.py 18443 https cert.pem key.pem
```

Routes (200000-byte body, byte i = `(i*7+3)&0xff`):

| Route | Response |
| --- | --- |
| `/full` | 200, Content-Length 200000, full body |
| `/trunc` | 200, Content-Length 200000, 90000 bytes then FIN |
| `/truncrst` | 200, Content-Length 200000, 90000 bytes then RST |
| `/chunk` | 200, chunked, full body |
| `/range` | 200 full body, or 206 partial with a Range header |
| `/small` | 200, Content-Length 1234 |

Expected: full and chunked fetches byte-match; truncated or reset responses
exit non-zero; a second run on a truncated file resumes to the full size.
