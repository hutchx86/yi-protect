/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 */
/* downloader: tiny static HTTP/HTTPS fetcher (mbedTLS 2.28) for BusyBox cameras.
 * Usage and CA-bundle search order: see README.md.
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <unistd.h>
#include <errno.h>
#include <sys/time.h>
#include <sys/socket.h>
#include <sys/stat.h>

#include "mbedtls/net_sockets.h"
#include "mbedtls/ssl.h"
#include "mbedtls/entropy.h"
#include "mbedtls/ctr_drbg.h"
#include "mbedtls/x509_crt.h"
#include "mbedtls/error.h"

#define DEFAULT_UA        "downloader/1.0"
#define MAX_REDIRECTS     6
#define RECV_TIMEOUT_SEC  120

typedef struct { char host[256]; char port[8]; char path[4096]; int https; } url_t;

typedef struct {
    mbedtls_net_context net;
    mbedtls_ssl_context ssl;
    int is_tls;
    int open;
} conn_t;

typedef struct {
    conn_t *c;
    unsigned char buf[8192];
    size_t pos, len;
    int eof;
    int err;   /* set when the underlying read failed (not a clean EOF) */
} br_t;

typedef struct {
    mbedtls_entropy_context  entropy;
    mbedtls_ctr_drbg_context drbg;
    mbedtls_x509_crt         cacert;
    int ca_loaded;
} tlsctx_t;

/* ---------------- URL parsing ---------------- */

static int parse_url(const char *s, url_t *u, char *err, size_t errsz)
{
    const char *p = strstr(s, "://");
    if (!p) { snprintf(err, errsz, "malformed URL (no scheme): %s", s); return -1; }
    int https;
    if (p - s == 5 && !strncasecmp(s, "https", 5))      https = 1;
    else if (p - s == 4 && !strncasecmp(s, "http", 4))  https = 0;
    else { snprintf(err, errsz, "unsupported scheme: %s", s); return -1; }

    const char *h     = p + 3;
    const char *slash = strchr(h, '/');
    const char *end   = slash ? slash : h + strlen(h);
    const char *colon = memchr(h, ':', (size_t)(end - h));

    if (colon) {
        size_t hl = (size_t)(colon - h);
        if (hl == 0 || hl >= sizeof(u->host)) { snprintf(err, errsz, "bad host"); return -1; }
        memcpy(u->host, h, hl); u->host[hl] = 0;
        size_t pl = (size_t)(end - colon - 1);
        if (pl == 0 || pl >= sizeof(u->port)) { snprintf(err, errsz, "bad port"); return -1; }
        memcpy(u->port, colon + 1, pl); u->port[pl] = 0;
    } else {
        size_t hl = (size_t)(end - h);
        if (hl == 0 || hl >= sizeof(u->host)) { snprintf(err, errsz, "bad host"); return -1; }
        memcpy(u->host, h, hl); u->host[hl] = 0;
        strcpy(u->port, https ? "443" : "80");
    }
    if (slash) {
        snprintf(u->path, sizeof(u->path), "%s", slash);
    } else {
        strcpy(u->path, "/");
    }
    u->https = https;
    return 0;
}

/* ---------------- connection + TLS ---------------- */

static void conn_close(conn_t *c)
{
    if (!c->open) return;
    if (c->is_tls)
        mbedtls_ssl_close_notify(&c->ssl);
    mbedtls_ssl_free(&c->ssl);
    mbedtls_net_free(&c->net);
    c->open = 0;
}

static int conn_open(conn_t *c, const url_t *u, tlsctx_t *t, int insecure)
{
    mbedtls_ssl_config conf;
    int ret;
    char ebuf[256];

    memset(c, 0, sizeof(*c));
    mbedtls_net_init(&c->net);
    mbedtls_ssl_init(&c->ssl);
    mbedtls_ssl_config_init(&conf);

    ret = mbedtls_net_connect(&c->net, u->host, u->port, MBEDTLS_NET_PROTO_TCP);
    if (ret != 0) {
        mbedtls_strerror(ret, ebuf, sizeof ebuf);
        fprintf(stderr, "downloader: connect %s:%s failed: %s\n", u->host, u->port, ebuf);
        goto fail;
    }
    {
        struct timeval tv;
        tv.tv_sec = RECV_TIMEOUT_SEC; tv.tv_usec = 0;
        setsockopt(c->net.fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
        setsockopt(c->net.fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
    }

    if (!u->https) {
        c->is_tls = 0;
        c->open   = 1;
        mbedtls_ssl_config_free(&conf);
        return 0;
    }

    c->is_tls = 1;

    ret = mbedtls_ssl_config_defaults(&conf, MBEDTLS_SSL_IS_CLIENT,
                                      MBEDTLS_SSL_TRANSPORT_STREAM,
                                      MBEDTLS_SSL_PRESET_DEFAULT);
    if (ret != 0) { mbedtls_strerror(ret, ebuf, sizeof ebuf);
                    fprintf(stderr, "downloader: ssl_config_defaults: %s\n", ebuf); goto fail; }

    mbedtls_ssl_conf_rng(&conf, mbedtls_ctr_drbg_random, &t->drbg);
    mbedtls_ssl_conf_min_version(&conf, MBEDTLS_SSL_MAJOR_VERSION_3,
                                 MBEDTLS_SSL_MINOR_VERSION_3); /* TLS 1.2 */

    if (insecure) {
        mbedtls_ssl_conf_authmode(&conf, MBEDTLS_SSL_VERIFY_NONE);
    } else {
        mbedtls_ssl_conf_authmode(&conf, MBEDTLS_SSL_VERIFY_REQUIRED);
        mbedtls_ssl_conf_ca_chain(&conf, &t->cacert, NULL);
    }

    ret = mbedtls_ssl_setup(&c->ssl, &conf);
    if (ret != 0) { mbedtls_strerror(ret, ebuf, sizeof ebuf);
                    fprintf(stderr, "downloader: ssl_setup: %s\n", ebuf); goto fail; }

    mbedtls_ssl_set_hostname(&c->ssl, u->host);
    mbedtls_ssl_set_bio(&c->ssl, &c->net, mbedtls_net_send, mbedtls_net_recv, NULL);

    while ((ret = mbedtls_ssl_handshake(&c->ssl)) != 0) {
        if (ret != MBEDTLS_ERR_SSL_WANT_READ && ret != MBEDTLS_ERR_SSL_WANT_WRITE) {
            mbedtls_strerror(ret, ebuf, sizeof ebuf);
            fprintf(stderr, "downloader: TLS handshake with %s failed: %s\n", u->host, ebuf);
            goto fail;
        }
    }

    if (!insecure) {
        uint32_t flags = mbedtls_ssl_get_verify_result(&c->ssl);
        if (flags != 0) {
            char vbuf[512];
            mbedtls_x509_crt_verify_info(vbuf, sizeof vbuf, "  ", flags);
            fprintf(stderr, "downloader: certificate verification failed for %s:\n%s\n",
                    u->host, vbuf);
            goto fail;
        }
    }

    c->open = 1;
    mbedtls_ssl_config_free(&conf);
    return 0;

fail:
    mbedtls_ssl_config_free(&conf);
    conn_close(c);
    return -1;
}

/* ---------------- buffered reads ---------------- */

static int conn_read(conn_t *c, unsigned char *buf, size_t len)
{
    if (c->is_tls) {
        int r;
        do { r = mbedtls_ssl_read(&c->ssl, buf, len); }
        while (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE);
        /* A peer close_notify is an orderly end of stream, not an error. */
        if (r == MBEDTLS_ERR_SSL_PEER_CLOSE_NOTIFY)
            return 0;
        return r;
    }
    for (;;) {
        int r = (int)read(c->net.fd, buf, len);
        if (r < 0 && errno == EINTR) continue;
        return r;
    }
}

static int conn_write(conn_t *c, const unsigned char *buf, size_t len)
{
    size_t off = 0;
    while (off < len) {
        int r;
        if (c->is_tls) {
            r = mbedtls_ssl_write(&c->ssl, buf + off, len - off);
            if (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE) continue;
        } else {
            r = (int)write(c->net.fd, buf + off, len - off);
        }
        if (r <= 0) return -1;
        off += (size_t)r;
    }
    return (int)off;
}

/* Returns 1 when a byte is available, 0 on a clean EOF, -1 on a read error. */
static int br_fill(br_t *b)
{
    if (b->pos < b->len) return 1;
    if (b->eof) return 0;
    if (b->err) return -1;
    int r = conn_read(b->c, b->buf, sizeof b->buf);
    if (r > 0) { b->pos = 0; b->len = (size_t)r; return 1; }
    if (r == 0) { b->eof = 1; return 0; }
    b->err = 1;
    return -1;
}

static int br_getc(br_t *b)
{
    if (br_fill(b) <= 0) return -1;
    return b->buf[b->pos++];
}

static size_t br_read(br_t *b, unsigned char *out, size_t n)
{
    size_t got = 0;
    while (got < n) {
        if (b->pos >= b->len && br_fill(b) <= 0) break;
        size_t avail = b->len - b->pos;
        size_t take  = n - got;
        if (take > avail) take = avail;
        memcpy(out + got, b->buf + b->pos, take);
        b->pos += take;
        got    += take;
    }
    return got;
}

static int read_line(br_t *b, char *line, size_t n)
{
    size_t i = 0;
    for (;;) {
        int ch = br_getc(b);
        if (ch < 0) { if (i == 0) return -1; break; }
        if (ch == '\n') break;
        if (ch == '\r') continue;
        if (i + 1 < n) line[i++] = (char)ch;
    }
    line[i] = 0;
    return (int)i;
}

/* ---------------- HTTP body ---------------- */

static int write_body(br_t *b, FILE *out, int chunked, long content_length)
{
    unsigned char tmp[16384];
    long total = 0;

    if (chunked) {
        char sz[128];
        for (;;) {
            if (read_line(b, sz, sizeof sz) < 0) return -1;
            char *ext = strchr(sz, ';');
            if (ext) *ext = 0;
            long cl = strtol(sz, NULL, 16);
            if (cl <= 0) {
                /* last chunk: consume trailer headers up to blank line */
                while (read_line(b, sz, sizeof sz) > 0) { }
                return 0;
            }
            long left = cl;
            while (left > 0) {
                size_t want = (size_t)(left < (long)sizeof tmp ? left : (long)sizeof tmp);
                size_t g = br_read(b, tmp, want);
                if (g == 0) return -1;
                if (fwrite(tmp, 1, g, out) != g) return -1;
                left -= (long)g;
                total += (long)g;
            }
            read_line(b, sz, sizeof sz); /* trailing CRLF */
        }
    }

    for (;;) {
        size_t g = br_read(b, tmp, sizeof tmp);
        if (g == 0) break;
        if (fwrite(tmp, 1, g, out) != g) return -1;
        total += (long)g;
        if (content_length >= 0 && total >= content_length) break;
    }
    if (b->err) return -1;
    if (content_length >= 0 && total != content_length) return -1;
    return 0;
}

/* ---------------- one request ---------------- */

static int fetch_once(const char *urlstr, FILE *out, tlsctx_t *t, int insecure,
                      long *resume, char *redir, size_t redirsz, int *is_redir,
                      char *err, size_t errsz)
{
    url_t u;
    conn_t c;
    br_t b;
    char req[8192];
    char line[1024];
    char location[4096] = "";
    long content_length = -1;
    long cr_start = -1;       /* Content-Range start (206) */
    long cr_total = -1;       /* Content-Range total (206 / 416) */
    int chunked = 0, status = 0;
    size_t off;

    *is_redir = 0;
    if (parse_url(urlstr, &u, err, errsz) != 0) return -1;
    if (conn_open(&c, &u, t, insecure) != 0) { snprintf(err, errsz, "connection/TLS failure"); return -1; }

    {
        const char *default_port = u.https ? "443" : "80";
        int hostport = strcmp(u.port, default_port) != 0;
        char rng[64] = "";
        if (*resume > 0)
            snprintf(rng, sizeof rng, "Range: bytes=%ld-\r\n", *resume);
        snprintf(req, sizeof req,
                 "GET %s HTTP/1.1\r\n"
                 "Host: %s%s%s\r\n"
                 "User-Agent: %s\r\n"
                 "Accept: */*\r\n"
                 "%s"
                 "Connection: close\r\n"
                 "\r\n",
                 u.path, u.host, hostport ? ":" : "", hostport ? u.port : "",
                 DEFAULT_UA, rng);
    }
    if (conn_write(&c, (const unsigned char *)req, strlen(req)) < 0) {
        snprintf(err, errsz, "failed to send request"); conn_close(&c); return -1;
    }

    memset(&b, 0, sizeof b);
    b.c = &c;

    if (read_line(&b, line, sizeof line) <= 0) {
        snprintf(err, errsz, "no HTTP status line"); conn_close(&c); return -1;
    }
    {
        char *sp = strchr(line, ' ');
        if (sp) status = atoi(sp + 1);
    }
    while (read_line(&b, line, sizeof line) > 0) {
        if (!strncasecmp(line, "Location:", 9)) {
            const char *v = line + 9;
            while (*v == ' ' || *v == '\t') v++;
            snprintf(location, sizeof location, "%s", v);
        } else if (!strncasecmp(line, "Content-Length:", 15)) {
            content_length = atol(line + 15);
        } else if (!strncasecmp(line, "Content-Range:", 14)) {
            /* bytes START-END/TOTAL  or  bytes star-slash TOTAL (416) */
            const char *v = line + 14;
            while (*v == ' ' || *v == '\t') v++;
            if (!strncasecmp(v, "bytes", 5)) {
                v += 5;
                while (*v == ' ') v++;
                if (*v == '*') {
                    const char *slash = strchr(v, '/');
                    if (slash) cr_total = atol(slash + 1);
                } else {
                    cr_start = atol(v);
                    const char *dash = strchr(v, '-');
                    const char *slash = dash ? strchr(dash, '/') : NULL;
                    if (slash) cr_total = atol(slash + 1);
                }
            }
        } else if (!strncasecmp(line, "Transfer-Encoding:", 18)) {
            if (strcasestr(line + 18, "chunked")) chunked = 1;
        }
    }

    if ((status == 301 || status == 302 || status == 303 || status == 307 || status == 308)
        && location[0]) {
        snprintf(redir, redirsz, "%s", location);
        *is_redir = 1;
        conn_close(&c);
        return 0;
    }

    if (status == 416 && *resume > 0 && cr_total == *resume) {
        /* The partial file already covers the whole resource. */
        conn_close(&c);
        return 0;
    }

    if (status == 206) {
        if (*resume <= 0 || (cr_start >= 0 && cr_start != *resume)) {
            snprintf(err, errsz, "bad partial response (Content-Range start %ld, expected %ld)",
                     cr_start, *resume);
            conn_close(&c);
            return -1;
        }
        if (out != stdout && fseek(out, *resume, SEEK_SET) != 0) {
            snprintf(err, errsz, "cannot seek output to %ld", *resume);
            conn_close(&c);
            return -1;
        }
    } else if (status == 200) {
        if (*resume > 0) {
            /* Server ignored Range: restart from scratch. */
            if (out != stdout &&
                (ftruncate(fileno(out), 0) != 0 || fseek(out, 0, SEEK_SET) != 0)) {
                snprintf(err, errsz, "cannot restart output file");
                conn_close(&c);
                return -1;
            }
            *resume = 0;
        }
    } else {
        snprintf(err, errsz, "HTTP status %d", status);
        conn_close(&c);
        return -1;
    }

    if (write_body(&b, out, chunked, content_length) != 0) {
        snprintf(err, errsz, "error while reading response body");
        conn_close(&c);
        return -1;
    }

    (void)off;
    conn_close(&c);
    return 0;
}

/* ---------------- CA bundle handling ---------------- */

static const char *ca_candidates[] = {
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/ssl/certs/ca-bundle.crt",
    "/etc/ssl/cert.pem",
    "/etc/ssl/ca-bundle.pem",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/usr/share/ca-certificates/cacert.pem",
    NULL
};

static const char *find_ca(const char *explicit_ca)
{
    const char *env;
    if (explicit_ca && access(explicit_ca, R_OK) == 0) return explicit_ca;
    if ((env = getenv("DOWNLOADER_CA")) && access(env, R_OK) == 0) return env;
    if ((env = getenv("SSL_CERT_FILE")) && access(env, R_OK) == 0) return env;
    if ((env = getenv("CURL_CA_BUNDLE")) && access(env, R_OK) == 0) return env;
    for (int i = 0; ca_candidates[i]; i++)
        if (access(ca_candidates[i], R_OK) == 0) return ca_candidates[i];
    return NULL;
}

static void usage(const char *prog)
{
    fprintf(stderr,
        "usage: %s [--insecure|-k|--no-check-certificate] [--ca FILE] <url> <outfile>\n"
        "\n"
        "  <url>       http:// or https:// URL to fetch\n"
        "  <outfile>   destination file ('-' for stdout)\n"
        "  -k, --insecure, --no-check-certificate\n"
        "              do not verify the server certificate (needed when no CA\n"
        "              bundle exists on the device)\n"
        "  --ca FILE   verify against this CA bundle\n",
        prog);
}

int main(int argc, char **argv)
{
    int insecure = 0;
    const char *ca_opt = NULL, *url = NULL, *outpath = NULL;
    tlsctx_t t;
    FILE *out;
    char cur[8192];
    int i, hop, rc = 1;
    long resume = 0;
    char err[512] = "";

    for (i = 1; i < argc; i++) {
        const char *a = argv[i];
        if (!strcmp(a, "-k") || !strcmp(a, "--insecure") ||
            !strcmp(a, "--no-check-certificate")) {
            insecure = 1;
        } else if (!strcmp(a, "--ca")) {
            if (++i >= argc) { fprintf(stderr, "downloader: --ca needs an argument\n"); return 2; }
            ca_opt = argv[i];
        } else if (!strcmp(a, "-h") || !strcmp(a, "--help")) {
            usage(argv[0]); return 0;
        } else if (a[0] == '-' && a[1] != '\0') {
            fprintf(stderr, "downloader: unknown option '%s'\n", a);
            usage(argv[0]); return 2;
        } else if (!url) {
            url = a;
        } else if (!outpath) {
            outpath = a;
        } else {
            fprintf(stderr, "downloader: too many arguments\n");
            usage(argv[0]); return 2;
        }
    }
    if (!url || !outpath) { usage(argv[0]); return 2; }

    memset(&t, 0, sizeof t);
    mbedtls_entropy_init(&t.entropy);
    mbedtls_ctr_drbg_init(&t.drbg);
    mbedtls_x509_crt_init(&t.cacert);

    {
        int ret = mbedtls_ctr_drbg_seed(&t.drbg, mbedtls_entropy_func, &t.entropy,
                                        (const unsigned char *)"downloader", 10);
        if (ret != 0) {
            char ebuf[256];
            mbedtls_strerror(ret, ebuf, sizeof ebuf);
            fprintf(stderr, "downloader: rng seed failed: %s\n", ebuf);
            goto done;
        }
    }

    if (!insecure) {
        const char *ca = find_ca(ca_opt);
        if (!ca) {
            fprintf(stderr,
                "downloader: no CA bundle found; refusing to verify.\n"
                "  Pass --ca FILE, set $SSL_CERT_FILE, or use -k/--insecure.\n");
            goto done;
        }
        if (mbedtls_x509_crt_parse_file(&t.cacert, ca) != 0) {
            fprintf(stderr, "downloader: failed to parse CA bundle: %s\n", ca);
            goto done;
        }
        t.ca_loaded = 1;
        fprintf(stderr, "downloader: verifying against CA bundle %s\n", ca);
    } else {
        fprintf(stderr, "downloader: certificate verification DISABLED (-k)\n");
    }

    if (!strcmp(outpath, "-")) {
        out = stdout;
    } else {
        /* Resume a partial file left by an earlier failed attempt. */
        struct stat st;
        if (stat(outpath, &st) == 0 && S_ISREG(st.st_mode) && st.st_size > 0)
            resume = (long)st.st_size;
        out = fopen(outpath, resume > 0 ? "r+b" : "wb");
        if (!out) {
            fprintf(stderr, "downloader: cannot open '%s': %s\n", outpath, strerror(errno));
            goto done;
        }
        if (resume > 0)
            fprintf(stderr, "downloader: resuming '%s' at byte %ld\n", outpath, resume);
    }

    snprintf(cur, sizeof cur, "%s", url);

    for (hop = 0; hop <= MAX_REDIRECTS; hop++) {
        char redir[4096] = "";
        int is_redir = 0;

        if (fetch_once(cur, out, &t, insecure, &resume, redir, sizeof redir, &is_redir,
                       err, sizeof err) != 0) {
            fprintf(stderr, "downloader: %s (%s)\n", err, cur);
            goto out_close;
        }
        if (!is_redir) break;

        fprintf(stderr, "downloader: redirect -> %s\n", redir);
        if (!strncmp(redir, "http://", 7) || !strncmp(redir, "https://", 8)) {
            snprintf(cur, sizeof cur, "%s", redir);
        } else {
            url_t base;
            char berr[128];
            if (parse_url(cur, &base, berr, sizeof berr) != 0) {
                fprintf(stderr, "downloader: bad redirect target\n");
                goto out_close;
            }
            const char *defport = base.https ? "443" : "80";
            const char *auth = strcmp(base.port, defport) ? base.port : NULL;
            if (redir[0] == '/') {
                snprintf(cur, sizeof cur, "%s://%s%s%s%s",
                         base.https ? "https" : "http", base.host,
                         auth ? ":" : "", auth ? auth : "", redir);
            } else {
                /* path-relative: resolve against the directory of base.path */
                char dir[4096];
                snprintf(dir, sizeof dir, "%s", base.path);
                char *sl = strrchr(dir, '/');
                if (sl) sl[1] = 0; else strcpy(dir, "/");
                snprintf(cur, sizeof cur, "%s://%s%s%s%s%s",
                         base.https ? "https" : "http", base.host,
                         auth ? ":" : "", auth ? auth : "", dir, redir);
            }
        }
    }

    if (fflush(out) != 0) {
        fprintf(stderr, "downloader: write error: %s\n", strerror(errno));
        goto out_close;
    }
    rc = 0;

out_close:
    if (out != stdout) fclose(out);
done:
    mbedtls_x509_crt_free(&t.cacert);
    mbedtls_ctr_drbg_free(&t.drbg);
    mbedtls_entropy_free(&t.entropy);
    return rc;
}
