#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
"""Local test server for the downloader regression tests (routes: README.md)."""
import os
import socket
import socketserver
import ssl
import struct
import sys

N = 200000
TRUNC = 90000
BODY = bytes(((i * 7 + 3) & 0xFF) for i in range(N))
SMALL = bytes(((i * 5 + 1) & 0xFF) for i in range(1234))


def header(status):
    return ("HTTP/1.1 %s\r\n" % status).encode()


class Handler(socketserver.StreamRequestHandler):
    def _send(self, data):
        self.wfile.write(data)
        self.wfile.flush()

    def _headers(self, status, extra):
        self._send(header(status) + b"Server: test\r\n" + extra +
                   b"Connection: close\r\n\r\n")

    def _rst(self):
        # Force a TCP RST instead of a graceful FIN.
        try:
            self.connection.setsockopt(
                socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
            self.connection.close()
        except OSError:
            pass

    def handle(self):
        line = self.rfile.readline(65536).decode("latin1").strip()
        if not line:
            return
        parts = line.split()
        path = parts[1] if len(parts) > 1 else "/"
        hdrs = {}
        while True:
            h = self.rfile.readline(65536)
            if h in (b"\r\n", b"\n", b""):
                break
            k, _, v = h.decode("latin1").partition(":")
            hdrs[k.strip().lower()] = v.strip()

        if path == "/full":
            self._headers("200 OK", b"Content-Length: %d\r\n" % N)
            self._send(BODY)
        elif path == "/small":
            self._headers("200 OK", b"Content-Length: %d\r\n" % len(SMALL))
            self._send(SMALL)
        elif path == "/trunc":
            self._headers("200 OK", b"Content-Length: %d\r\n" % N)
            self._send(BODY[:TRUNC])
            # graceful FIN with body short of Content-Length
        elif path == "/truncrst":
            self._headers("200 OK", b"Content-Length: %d\r\n" % N)
            self._send(BODY[:TRUNC])
            self._rst()
            return
        elif path == "/chunk":
            self._headers("200 OK", b"Transfer-Encoding: chunked\r\n")
            step = 4096
            for off in range(0, N, step):
                piece = BODY[off:off + step]
                self._send(b"%x\r\n" % len(piece) + piece + b"\r\n")
            self._send(b"0\r\n\r\n")
        elif path == "/range":
            rng = hdrs.get("range")
            start = 0
            if rng and rng.lower().startswith("bytes="):
                spec = rng[6:].split("-")[0].strip()
                if spec:
                    start = int(spec)
            if 0 < start < N:
                part = BODY[start:]
                self._headers("206 Partial Content",
                              b"Content-Length: %d\r\n"
                              b"Content-Range: bytes %d-%d/%d\r\n"
                              % (len(part), start, N - 1, N))
                self._send(part)
            elif start >= N:
                self._headers("416 Range Not Satisfiable",
                              b"Content-Range: bytes */%d\r\n" % N)
            else:
                self._headers("200 OK", b"Content-Length: %d\r\n" % N)
                self._send(BODY)
        else:
            self._headers("404 Not Found", b"Content-Length: 0\r\n")


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    port = int(sys.argv[1])
    mode = sys.argv[2]
    srv = Server(("127.0.0.1", port), Handler)
    if mode == "https":
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(sys.argv[3], sys.argv[4])
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    print("serving %s on 127.0.0.1:%d" % (mode, port), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
