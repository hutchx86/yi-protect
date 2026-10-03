// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// manage.go implements the camera-side HTTPS management API on :443, the
// controller-initiated adopt push (both the 1.2 and older 1.1 flows).
package main

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

// manageResult is a successful controller-initiated adopt push, whichever API
// shape delivered it.
type manageResult struct {
	Token string
	Host  string
	// ConsoleID is the adopting controller's own stable identity
	// (mgmt.consoleId in the /api/1.2/manage flow); empty for 1.1.
	ConsoleID string
}

// manageCh delivers a manageResult from the HTTP handlers to main()'s dial
// loop. Buffered by 1: only the most recent push matters.
var manageCh = make(chan manageResult, 1)

// awaitingManage mirrors the dial-loop state: true means "do not dial :7442 --
// wait passively for a manage push", matching real hardware after a reset.
var awaitingManage atomic.Bool

// manageAwaitFilePath persists awaitingManage across the ResetToDefaults
// reboot; main() checks it on startup.
var manageAwaitFilePath = yipPrefix + "/etc/yi_protect_client_go.awaiting-manage"

// enterAwaitingManage flips the in-memory flag and persists it.
func enterAwaitingManage(reason string) {
	awaitingManage.Store(true)
	if err := os.WriteFile(manageAwaitFilePath, []byte(reason+"\n"), 0600); err != nil {
		log.Printf("enterAwaitingManage: failed to persist state: %v", err)
	}
	log.Printf("entering awaiting-manage state (%s): will not dial :7442 until a controller adopt push arrives on :443", reason)
}

// leaveAwaitingManage is the reverse, called once a manage push is applied.
func leaveAwaitingManage() {
	awaitingManage.Store(false)
	if err := os.Remove(manageAwaitFilePath); err != nil && !os.IsNotExist(err) {
		log.Printf("leaveAwaitingManage: failed to clear persisted state: %v", err)
	}
}

// runManageServer runs the HTTPS listener forever. Started unconditionally
// (real hardware's lighttpd is always up); errors are logged, not fatal.
func runManageServer() {
	certPEM, keyPEM, err := generateSelfSignedECDSACert("yi-hack-cam-mgmt", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		log.Printf("manage API: failed to generate server cert, listener not started: %v", err)
		return
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("manage API: failed to load generated cert, listener not started: %v", err)
		return
	}

	srv := &http.Server{
		Addr:      ":443",
		Handler:   manageHTTPHandler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	log.Printf("manage API: listening on :443 (POST /api/1.0|1.2/login, PUT /api/1.1/settings, POST /api/1.2/manage)")
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Printf("manage API: listener stopped: %v", err)
	}
}

func manageHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/1.0/login", handleLogin)
	mux.HandleFunc("/api/1.2/login", handleLogin)
	mux.HandleFunc("/api/1.1/settings", handleSettingsV1)
	mux.HandleFunc("/api/1.1/status", handleStatusV1)
	mux.HandleFunc("/api/1.2/manage", handleManageV2)
	return mux
}

// handleLogin deliberately accepts anything: the only caller is the controller
// on a private LAN.
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	log.Printf("manage API: %s from %s: %s", r.URL.Path, r.RemoteAddr, string(body))

	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: hex.EncodeToString(tok), Path: "/"})
	w.WriteHeader(http.StatusOK)
}

// handleSettingsV1 implements PUT /api/1.1/settings, the older
// login+settings+status adoption flow.
func handleSettingsV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	log.Printf("manage API: PUT /api/1.1/settings from %s: %s", r.RemoteAddr, string(body))

	var req struct {
		Controller struct {
			Addr  string `json:"addr"`
			Token string `json:"token"`
		} `json:"controller"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Controller.Addr == "" {
		http.Error(w, "missing controller.addr", http.StatusBadRequest)
		return
	}
	deliverManagePush(manageResult{Token: req.Controller.Token, Host: stripPort(req.Controller.Addr)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func handleStatusV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := "PROVISIONING"
	if !awaitingManage.Load() {
		state = "CONNECTING"
	}
	resp := map[string]interface{}{
		"controller": map[string]interface{}{
			"addr":  cfg.Host,
			"state": state,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleManageV2 implements POST /api/1.2/manage, the confirmed flow.
func handleManageV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	log.Printf("manage API: POST /api/1.2/manage from %s: %s", r.RemoteAddr, string(body))

	var req struct {
		Mgmt struct {
			Token     string   `json:"token"`
			Hosts     []string `json:"hosts"`
			Protocol  string   `json:"protocol"`
			ConsoleID string   `json:"consoleId"`
		} `json:"mgmt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Mgmt.Hosts) == 0 {
		http.Error(w, "missing mgmt.hosts", http.StatusBadRequest)
		return
	}
	deliverManagePush(manageResult{
		Token:     req.Mgmt.Token,
		Host:      stripPort(req.Mgmt.Hosts[0]),
		ConsoleID: req.Mgmt.ConsoleID,
	})
	w.WriteHeader(http.StatusOK)
}

// deliverManagePush hands a freshly-received controller push to main()'s dial
// loop and immediately clears awaitingManage -- real hardware dials right away.
func deliverManagePush(res manageResult) {
	leaveAwaitingManage()
	// Only the newest push matters: drop a stale buffered one, don't block.
	select {
	case <-manageCh:
	default:
	}
	manageCh <- res
	log.Printf("manage push accepted: host=%s token=%q -- signaling dial loop", res.Host, res.Token)
}

func stripPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}
