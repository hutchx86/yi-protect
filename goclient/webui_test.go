// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		{"POST", "/api/denoise", `{"key":"venc3d","value":512}`, http.StatusBadRequest},
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

func TestWebuiTimezoneOverride(t *testing.T) {
	dir := t.TempDir()
	oldOverride, oldPosix := timezoneOverridePath, timezonePosixPath
	timezoneOverridePath = filepath.Join(dir, "override")
	timezonePosixPath = filepath.Join(dir, "tz")
	defer func() {
		timezoneOverridePath, timezonePosixPath = oldOverride, oldPosix
		timezoneOverride.Store("")
		controllerTimezone.Store("")
	}()

	h := webuiHandler()
	cases := []struct {
		method, body string
		want         int
	}{
		{"GET", "", http.StatusOK},
		{"POST", `not json`, http.StatusBadRequest},
		{"POST", `{"override":"Bogus/Zone"}`, http.StatusBadRequest},
		{"POST", `{"override":"Europe/Berlin"}`, http.StatusOK},
		{"POST", `{"override":""}`, http.StatusOK},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, "/api/timezone", strings.NewReader(c.body)))
		if rr.Code != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.body, rr.Code, c.want)
		}
	}
	if got := effectiveTimezoneRaw(); got != "" {
		t.Errorf("cleared override still effective: %q", got)
	}
}

func TestTimezoneOverrideWinsOverController(t *testing.T) {
	controllerTimezone.Store("Europe/Berlin")
	timezoneOverride.Store("America/New_York")
	defer func() {
		controllerTimezone.Store("")
		timezoneOverride.Store("")
	}()

	if got := effectiveTimezoneRaw(); got != "America/New_York" {
		t.Errorf("effective = %q, want the override", got)
	}
	// The controller's ack still reports what Protect sent, not the override.
	if got := currentTimezone(); got != "Europe/Berlin" {
		t.Errorf("ack timezone = %q, want Europe/Berlin", got)
	}

	timezoneOverride.Store("")
	if got := effectiveTimezoneRaw(); got != "Europe/Berlin" {
		t.Errorf("effective after clear = %q, want the controller's value", got)
	}
}
