// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebuiRoutes(t *testing.T) {
	h := webuiHandler()
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/", "", http.StatusOK},
		{"GET", "/index.html", "", http.StatusOK},
		{"POST", "/", "", http.StatusMethodNotAllowed},
		{"POST", "/api/denoise", `{"key":"brightness","value":1}`, http.StatusBadRequest},
		{"POST", "/api/denoise", `{"key":"venc3d","value":4}`, http.StatusBadRequest},
		{"POST", "/api/denoise", `{"key":"tdf","value":-1}`, http.StatusBadRequest},
		{"POST", "/api/denoise", `not json`, http.StatusBadRequest},
		{"POST", "/api/denoise/unpin", `{"key":"gamma"}`, http.StatusBadRequest},
		{"GET", "/api/denoise/unpin", "", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rr.Code != c.want {
			t.Errorf("%s %s %s: got %d, want %d", c.method, c.path, c.body, rr.Code, c.want)
		}
	}
}

func TestWebuiPageListsEveryDenoiser(t *testing.T) {
	for _, k := range []string{"nr2d", "cnr", "tdf", "venc3d", "denoise"} {
		if _, ok := webuiFind(k); !ok {
			t.Errorf("denoiser %q missing from the page", k)
		}
	}
}
