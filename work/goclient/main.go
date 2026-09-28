// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// unifi-avclient: UniFi Protect "avclient" adoption/control client for
// Yi-Hack-Allwinner-v2; protocol shapes ported from unifi-cam-proxy.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// unifiPrefix is the project-owned root on the SD card; everything this client
// persists or shells out to lives under here.
const unifiPrefix = "/tmp/sd/unifi"

// Codec ALSA capture-gain control applied by setMicLevel (via bin/mixer_set):
// "MIC1 gain volume" on hw:0, range 0..31 (0 dB at 30), upstream of the encoder.
const (
	micGainCard    = "hw:0"
	micGainControl = "MIC1 gain volume"
)

type Config struct {
	Host      string
	Port      int
	Token     string
	MAC       string
	IP        string
	Model     string
	FWVersion string
	CertFile  string
	KeyFile   string
	// SysID is the real UBNT catalog hex system id for cfg.Model (0xa590 for
	// "UVC G3 Instant"), sent as `camera-model` and discovery's 0x10 TLV.
	SysID uint16

	// HasPTZ gates the "ptz" featureFlags key -> real hardware capability.
	// Default false; set via -ptz.
	HasPTZ bool

	// IsMediad gates the advanced picture-control path: when true AND a running
	// mediad is detected, picture settings go to its control socket (mediad_ctl.go).
	IsMediad bool

	// Mediad3DNR lets Protect's enable3dnr drive mediad's tdf; default false
	// leaves tdf to mediad's own default/config (Protect re-sends enable3dnr=1
	// on every connect, which would otherwise override the settings page).
	Mediad3DNR bool
	// WebUIPort is the firmware settings page's HTTP port (unifi.cfg
	// WEBUI_PORT, default 80; 0 disables it). See webui.go.
	WebUIPort int
	// ProtectSSH lets the controller's device credential log in over SSH
	// (unifi.cfg PROTECT_SSH, default false: UpdateUsernamePassword is only
	// acked). See ssh_credential.go.
	ProtectSSH bool
}

// cfg.MAC/cfg.IP are last-resort fallbacks used only when detectNetworkIdentity
// can't read wlan0/eth0; deliberately fake -- never put a real identity here.
var cfg = Config{
	Host:  "10.0.0.1",
	Port:  7442,
	Token: "", // set via -token flag
	MAC:   "DEADDEADBEEF",
	IP:    "0.0.0.0",
	Model: "UVC G3 Instant",
	// Model maps to platform SAV532Q; the suffix is not the real release, but a
	// version newer than any known build would make Protect offer a downgrade.
	FWVersion: "UVC.SAV532Q.v4.75.62.67.9cdac69.260331.1630",
	CertFile:  unifiPrefix + "/etc/unifi_client_go.crt",
	KeyFile:   unifiPrefix + "/etc/unifi_client_go.key",
	SysID:     0xa590,
}

// cfgMu guards cfg against the one real concurrent access: applyManagePush
// mutates it from main()'s goroutine while runUpdatesConnection reads it.
var cfgMu sync.Mutex

// cfgSnapshot returns a copy of cfg safe to read from runUpdatesConnection's
// goroutine.
func cfgSnapshot() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg
}

// deviceIDStr is the stable per-device UUID (loadOrCreateAdoptionUUID) -- the
// `device-id` WSS header and, once adopted, discovery's 0x26 TLV.
var deviceIDStr string

// deviceIDFilePath remembers where deviceIDStr was loaded from so
// deleteIdentity() can remove it.
var deviceIDFilePath string

// adoptionUUIDFilePath persists the controller's consoleId (discovery TLV
// 0x26) so a restart while adopted doesn't look "adopted to another console".
var adoptionUUIDFilePath = unifiPrefix + "/etc/unifi_client_go.adoption-uuid"

// loadAdoptionUUID restores the persisted 0x26 TLV value, if any. Absence or a
// malformed value leaves adoptionUUID as-is.
func loadAdoptionUUID(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return
	}
	raw, err := parseUUID(s)
	if err != nil {
		log.Printf("adoption-uuid file %q: invalid UUID %q: %v", path, s, err)
		return
	}
	adoptionUUID = raw[:]
	log.Printf("adoption-uuid loaded: %s", s)
}

// saveAdoptionUUID persists the 0x26 TLV value so it survives restarts.
func saveAdoptionUUID(path, s string) {
	if err := os.WriteFile(path, []byte(s+"\n"), 0600); err != nil {
		log.Printf("failed to persist adoption-uuid: %v", err)
	}
}

type Envelope struct {
	From             string                 `json:"from"`
	To               string                 `json:"to"`
	FunctionName     string                 `json:"functionName"`
	InResponseTo     int                    `json:"inResponseTo"`
	MessageID        int                    `json:"messageId"`
	Payload          map[string]interface{} `json:"payload"`
	ResponseExpected bool                   `json:"responseExpected"`
}

type Client struct {
	ws      *websocket.Conn
	msgID   int
	startTS time.Time
	// sendMu guards msgID and every ws.WriteJSON call; gorilla/websocket
	// requires all writes to be externally synchronized.
	sendMu    sync.Mutex
	streamsMu sync.Mutex
	streams   map[string]string // stream key (video1/2/3) -> assigned streamName, once a real destination is set

	// Active FlvPush destination/token per video stream; an unconditional
	// reconnect-per-message killed a healthy stream every ~10-15s while viewing.
	videoMu                sync.Mutex
	activeVideo1Host       string
	activeVideo1StreamName string
	activeVideo2Host       string
	activeVideo2StreamName string
	// video3 ("medium", what Auto live view picks) carries the SAME real LOW
	// frames as video2; HQ/LQ request video1/video2 directly.
	activeVideo3Host       string
	activeVideo3StreamName string

	// mTLS-enabled HTTP client (same cert as the WSS connection) for
	// snapshot uploads -- see handleGetRequest().
	httpClient *http.Client

	// snapshotGrabMu serializes imggrabber runs; concurrent forks race for the
	// shared sensor/ISP and OOM-kill rmm on this 60MB board.
	snapshotGrabMu sync.Mutex
	// snapshotMu guards the last-good JPEG cache below -- a slightly-stale
	// frame is better than no thumbnail when a grab misses its keyframe.
	snapshotMu   sync.Mutex
	snapshotJPEG map[string][]byte
	snapshotAt   map[string]time.Time

	// Microphone state from ChangeVideoSettings' `audio` block; micVolume keeps
	// the raw value, micBitRate the controller's requested audio bitrate (echoed
	// in the reply so its quality modes reconcile), micLevel the remapped
	// effective level, suppressing repeats.
	micMu       sync.Mutex
	micVolume   int
	micBitRate  int
	micLevel    int
	micLevelSet bool
}

// newUUIDv4 hand-rolls a random RFC 4122 v4 UUID -- no stdlib package, and one
// function isn't worth a dependency for a static cross-compiled binary.
func newUUIDv4() (string, [16]byte, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", b, err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return s, b, nil
}

// deviceIDNamespace is an arbitrary fixed namespace UUID for the UUIDv5
// derivation; constant so the same serial derives the same device-id.
var deviceIDNamespace = [16]byte{0x8b, 0x1a, 0x9d, 0x53, 0x6c, 0x4a, 0x40, 0x8e, 0xb0, 0x3a, 0xd8, 0x4c, 0xf4, 0x0d, 0x1e, 0x6f}

// newUUIDv5 derives a deterministic v5 UUID from name, so the same physical
// camera always derives the same device-id from its own hardware serial.
func newUUIDv5(name string) (string, [16]byte) {
	h := sha1.New()
	h.Write(deviceIDNamespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return s, b
}

// readHardwareSerial reads this camera's factory serial: the "mfg@<partition>"
// token in the kernel bootargs, then 20 bytes at offset 36 of that device.
func readHardwareSerial() (string, error) {
	bootargs, err := os.ReadFile("/sys/firmware/devicetree/base/chosen/bootargs")
	if err != nil {
		return "", fmt.Errorf("read bootargs: %w", err)
	}
	// Stop at whitespace or ':' -- the partitions= list is "name@dev:name@dev",
	// so a greedy \S+ would swallow the entries that follow mfg@.
	m := regexp.MustCompile(`mfg@([^\s:]+)`).FindSubmatch(bootargs)
	if m == nil {
		return "", fmt.Errorf("no mfg@ partition token in bootargs")
	}
	part := strings.TrimSpace(string(m[1]))

	f, err := os.Open("/dev/" + part)
	if err != nil {
		return "", fmt.Errorf("open /dev/%s: %w", part, err)
	}
	defer f.Close()

	buf := make([]byte, 20)
	if _, err := f.ReadAt(buf, 36); err != nil && err != io.EOF {
		return "", fmt.Errorf("read serial from /dev/%s: %w", part, err)
	}
	for i, c := range buf {
		if c == 0 {
			buf[i] = '0' // matches service.sh's `tr '\0' '0'`
		}
	}
	serial := strings.TrimSpace(string(buf))
	if serial == "" {
		return "", fmt.Errorf("empty serial read from /dev/%s", part)
	}
	return serial, nil
}

// modelSuffixFilePath records the real physical camera model (y623/h52ga/...),
// distinct from the spoofed cfg.Model/cfg.SysID.
const modelSuffixFilePath = unifiPrefix + "/etc/model_suffix"

// readHardwareModel returns this camera's physical model, falling back to "y623"
// if the file is missing or empty.
func readHardwareModel() string {
	if b, err := os.ReadFile(modelSuffixFilePath); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "y623"
}

// modelTablePath is the single per-model definition file (model sensor
// ring_offset ring_header high_w high_h ptz high_bitrate); one binary serves
// every model.
const modelTablePath = unifiPrefix + "/etc/model_table"

// modelDef is this client's slice of a model_table row.
type modelDef struct {
	highWidth   int
	highHeight  int
	ptz         bool
	highBitrate int // HIGH-channel bitrate to pin (bps); 0 = follow controller
}

// readModelDef returns the model_table row for `model`. ok is false when the
// table or the row is missing; the caller then keeps conservative defaults.
func readModelDef(model string) (def modelDef, ok bool) {
	b, err := os.ReadFile(modelTablePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("model_table %q unreadable: %v; using defaults", modelTablePath, err)
		}
		return modelDef{highWidth: 2304, highHeight: 1296}, false
	}
	def, ok = parseModelDef(string(b), model)
	if !ok {
		log.Printf("model_table: model %q not listed; using defaults (2304x1296, no PTZ)", model)
	}
	return def, ok
}

// parseModelDef finds `model`'s row in the model_table text. Columns: model
// sensor ring_offset ring_header high_w high_h ptz [high_bitrate], where the
// trailing high_bitrate (bps) is optional; 0/absent means follow the controller.
func parseModelDef(content, model string) (def modelDef, ok bool) {
	def = modelDef{highWidth: 2304, highHeight: 1296}
	for _, line := range strings.Split(content, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 7 || f[0] != model {
			continue
		}
		if w, e1 := strconv.Atoi(f[4]); e1 == nil {
			if h, e2 := strconv.Atoi(f[5]); e2 == nil && w > 0 && h > 0 {
				def.highWidth, def.highHeight = w, h
			}
		}
		def.ptz = strings.EqualFold(f[6], "yes")
		if len(f) >= 8 {
			if b, err := strconv.Atoi(f[7]); err == nil && b > 0 {
				def.highBitrate = b
			}
		}
		return def, true
	}
	return def, false
}

// modelHighBitrate returns the model's pinned HIGH-channel bitrate (bps), or 0
// when the model follows the controller (model_table high_bitrate column).
func modelHighBitrate() int {
	def, _ := readModelDef(readHardwareModel())
	return def.highBitrate
}

// readHighResolution returns the real HIGH-channel (video1) encoder geometry
// for this physical camera, from the model table.
func readHighResolution() (int, int) {
	def, _ := readModelDef(readHardwareModel())
	return def.highWidth, def.highHeight
}

// flagSet reports whether the named flag was given on the command line, so an
// explicit flag can be layered over config-file and model-table defaults.
func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// loadOrCreateAdoptionUUID returns a stable per-device UUID (string and raw
// bytes), persisted so it survives restarts.
func loadOrCreateAdoptionUUID(path string) (string, [16]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(data))
		if raw, perr := parseUUID(s); perr == nil {
			return s, raw, nil
		}
		log.Printf("device-id file %s unreadable (%v), regenerating", path, err)
	}

	// Prefer deriving from the hardware serial (unique, reproducible) over a
	// random UUID; random is the fallback when the serial can't be read.
	if serial, err := readHardwareSerial(); err == nil {
		s, raw := newUUIDv5(serial)
		if err := os.WriteFile(path, []byte(s+"\n"), 0600); err != nil {
			log.Printf("warning: failed to persist device-id to %s: %v", path, err)
		}
		log.Printf("device-id derived from hardware serial %q", serial)
		return s, raw, nil
	} else {
		log.Printf("could not read hardware serial (%v), falling back to a random device-id", err)
	}

	s, raw, err := newUUIDv4()
	if err != nil {
		return "", raw, err
	}
	if err := os.WriteFile(path, []byte(s+"\n"), 0600); err != nil {
		log.Printf("warning: failed to persist device-id to %s: %v", path, err)
	}
	return s, raw, nil
}

// generateSelfSignedECDSACert makes a fresh self-signed ECDSA P-256 pair, PEM-
// encoded; MUST stay ECDSA (an RSA-2048 keygen hung for minutes on this SoC).
func generateSelfSignedECDSACert(cn string, extUsage []x509.ExtKeyUsage) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  extUsage,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

// deleteIdentity removes the cert/key pair and derived device-id so a
// factory-reset device re-provisions fresh on next boot.
func (c *Client) deleteIdentity() error {
	for _, p := range []string{cfg.CertFile, cfg.KeyFile, deviceIDFilePath, adoptionUUIDFilePath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	adoptionUUID = nil
	setAdopted(false)

	log.Printf("deleteIdentity: removed cert/key/device-id; will re-provision on next boot")
	return nil
}

// ensureCertKey generates a fresh self-signed mTLS cert/key pair if either file
// is missing; idempotent -- an existing pair is left untouched.
func ensureCertKey(certPath, keyPath string) error {
	if _, certErr := os.Stat(certPath); certErr == nil {
		if _, keyErr := os.Stat(keyPath); keyErr == nil {
			return nil
		}
	}
	certPEM, keyPEM, err := generateSelfSignedECDSACert("yi-hack-cam", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	log.Printf("ensureCertKey: generated fresh cert/key at %s/%s", certPath, keyPath)
	return nil
}

// performReset deletes identity off the read loop, switches to awaiting-manage
// (manage.go) and reboots -- the gate that stops instant re-adoption by MAC.
func (c *Client) performReset() {
	if err := c.deleteIdentity(); err != nil {
		log.Printf("ResetToDefaults: failed to delete identity: %v", err)
	}

	enterAwaitingManage("ResetToDefaults")

	// If the reboot fails this process keeps running; awaitingManage gates the
	// dial loop. Clearing cfg.Token just keeps state consistent in that case.
	cfg.Token = ""

	log.Printf("ResetToDefaults: rebooting device now")
	if err := exec.Command("/sbin/reboot").Run(); err != nil {
		log.Printf("ResetToDefaults: reboot command failed: %v -- falling back to reconnect only", err)
		c.ws.Close()
	}
}

func parseUUID(s string) ([16]byte, error) {
	var out [16]byte
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return out, fmt.Errorf("not a 32-hex-char UUID: %q", s)
	}
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return out, err
	}
	copy(out[:], raw)
	return out, nil
}

func (c *Client) nextID() int {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.msgID++
	return c.msgID
}

func (c *Client) genResponse(name string, responseTo int, payload map[string]interface{}) Envelope {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	return Envelope{
		From:             "ubnt_avclient",
		To:               "UniFiVideo",
		FunctionName:     name,
		InResponseTo:     responseTo,
		MessageID:        c.nextID(),
		Payload:          payload,
		ResponseExpected: false,
	}
}

func (c *Client) send(e Envelope) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.ws.WriteJSON(e)
}

func (c *Client) uptime() float64 {
	return time.Since(c.startTS).Seconds()
}

type wifiStatus struct {
	essid          string
	bssid          string
	frequencyMHz   int
	channel        int
	signalLevelDBm int
	linkSpeedMbps  int
}

var (
	reWifiESSID   = regexp.MustCompile(`ESSID:"([^"]*)"`)
	reWifiFreq    = regexp.MustCompile(`Frequency:([0-9.]+) GHz`)
	reWifiBSSID   = regexp.MustCompile(`Access Point:\s*([0-9A-Fa-f:]{17})`)
	reWifiBitRate = regexp.MustCompile(`Bit Rate[:=]([0-9.]+) Mb/s`)
	reWifiSignal  = regexp.MustCompile(`Signal level[:=](-?[0-9]+) dBm`)
)

// readWifiStatus parses live iwconfig association state off wlan0, or nil if
// unassociated (a wired build) rather than fabricating values.
func readWifiStatus() *wifiStatus {
	out, err := exec.Command("/usr/sbin/iwconfig", "wlan0").Output()
	if err != nil {
		return nil
	}
	s := string(out)
	essidM := reWifiESSID.FindStringSubmatch(s)
	freqM := reWifiFreq.FindStringSubmatch(s)
	bssidM := reWifiBSSID.FindStringSubmatch(s)
	rateM := reWifiBitRate.FindStringSubmatch(s)
	sigM := reWifiSignal.FindStringSubmatch(s)
	if essidM == nil || freqM == nil || bssidM == nil || sigM == nil {
		return nil
	}
	freqGHz, err := strconv.ParseFloat(freqM[1], 64)
	if err != nil {
		return nil
	}
	freqMHz := int(freqGHz*1000 + 0.5)
	channel := 0
	switch {
	case freqMHz >= 2412 && freqMHz <= 2484:
		channel = (freqMHz - 2407) / 5
	case freqMHz >= 5000:
		channel = (freqMHz - 5000) / 5
	}
	signal, _ := strconv.Atoi(sigM[1])
	linkSpeed := 0
	if rateM != nil {
		if r, err := strconv.ParseFloat(rateM[1], 64); err == nil {
			linkSpeed = int(r + 0.5)
		}
	}
	return &wifiStatus{
		essid:          essidM[1],
		bssid:          strings.ToLower(bssidM[1]),
		frequencyMHz:   freqMHz,
		channel:        channel,
		signalLevelDBm: signal,
		linkSpeedMbps:  linkSpeed,
	}
}

// encoderFps is mediad's fixed capture/encode rate (SRC_FPS). Every fps field
// declares it: the controller adopts fps/validFpsValues from our reply.
const encoderFps = 20

// featureFlags' keys are read by service.js as hasX:Boolean(t.<key>);
// "truedaynight" is the real IR key, "ledStatus" the separate status LED.
func featureFlags() map[string]interface{} {
	flags := map[string]interface{}{
		"mic":          true,
		"speaker":      true,
		"truedaynight": true,
		"ledIR":        true,
		"ledStatus":    true, // hasLedStatus <- t.ledStatus; real hardware, ipc_cmd -l
		"wifi":         true, // hasWifi <- t.wifi; real hardware capability (8189fs module)
		// bluetooth/hdr/privacyMask/autoICROnly: real G3 Instant declares all
		// four true; self-declared metadata only, no behavior wired up.
		"bluetooth":    true,
		"hdr":          true,
		"privacyMask":  true,
		"autoICROnly":  true,
		"aec":          []string{"wideband"}, // feature_aec_wideband=1 on real hardware; narrowband/fullband both 0
		"videoMode":    []string{"default", "sport", "slowShutter"},
		"motionDetect": []string{"enhanced"},

		// Copied from a real G3 Instant's features; their absence made Protect
		// show empty codec lists and gated the talkback button.
		"audioCodecs":     []string{"aac", "opus"},
		"videoCodecs":     []string{"h264", "mjpg"},
		"opusSampleRates": []int{16000},
		// true on the G3 Instant we spoof (features_0xa590.json); the generic
		// features.json ships false. Talkback needs AEC to be armed (the real
		// streamer logs "Device have AEC, force to set withTalkback"), and a
		// false here is what left the talkback button greyed out / unarmed.
		"aecTalkbackSwitch": true,
		"videoSourceCount":  1,
		"videoModeMaxFps":   []int{encoderFps, encoderFps, encoderFps},
		// 0 on a real G3 Instant; absent (null) makes the controller log "has no
		// max scale down level, cannot calculate down-scale mode" on every connect.
		"maxScaleDownLevel":     0,
		"squareEventThumbnail":  true,
		"luxCheck":              false,
		"flash":                 false,
		"fisheye":               false,
		"presetTour":            false,
		"fullHdSnapshot":        false,
		"supportCustomRingtone": false,
		"verticalFlipWarning":   false,
		"privacyMasks": map[string]interface{}{
			"maxZones":      16,
			"rectangleOnly": false,
		},
		"hotplug": map[string]interface{}{
			"extender": map[string]interface{}{"attached": false},
		},
	}
	if cfg.HasPTZ {
		// hasPtz <- t.ptz; gates the Protect UI's PTZ controls independent of
		// model/platform. See ptz.go for the ipc_cmd surface this maps to.
		flags["ptz"] = true
	}
	return flags
}

// talkbackSettingsDefaults mirrors a real camera's values; samplingRate is this
// device's native PCM rate (16000) -- audio_in_fifo takes raw PCM, no resampling.
func talkbackSettingsDefaults() map[string]interface{} {
	return map[string]interface{}{
		"typeFmt": "aac", "typeIn": "serverudp",
		"bindAddr": "0.0.0.0", "bindPort": 7004,
		"filterAddr": nil, "filterPort": nil,
		"channels": 1, "samplingRate": 16000, "bitsPerSample": 16,
		"quality": 100,
	}
}

func (c *Client) initAdoption() error {
	payload := map[string]interface{}{
		"adoptionCode":         cfg.Token,
		"connectionHost":       cfg.Host,
		"connectionSecurePort": cfg.Port,
		"fwVersion":            cfg.FWVersion,
		"hwrev":                19,
		"idleTime":             0,
		"ip":                   cfg.IP,
		"mac":                  cfg.MAC,
		"model":                cfg.Model,
		"name":                 getDeviceName(),
		"protocolVersion":      67,
		"rebootTimeoutSec":     30,
		"semver":               "v4.4.8",
		"totalLoad":            0.1,
		"upgradeTimeoutSec":    150,
		"uptime":               int(c.uptime()),
		"features":             featureFlags(),
	}
	return c.send(c.genResponse("ubnt_avclient_hello", 0, payload))
}

func ispSettingsDefaults() map[string]interface{} {
	return map[string]interface{}{
		"aeMode": "auto", "aeTargetPercent": 50, "aggressiveAntiFlicker": 0,
		"brightness": 50, "contrast": 50, "criticalTmpOfProtect": 40,
		"darkAreaCompensateLevel": 0, "denoise": 50, "enable3dnr": 1,
		"enableMicroTmpProtect": 1, "enablePauseMotion": 0, "flip": 0,
		"focusMode": "ztrig", "focusPosition": 0, "forceFilterIrSwitchEvents": 0,
		"hue": 50, "icrLightSensorNightThd": 0, "icrSensitivity": 0,
		"irLedLevel": 215, "irLedMode": "auto",
		"irOnStsBrightness": 0, "irOnStsContrast": 0, "irOnStsDenoise": 0,
		"irOnStsHue": 0, "irOnStsSaturation": 0, "irOnStsSharpness": 0, "irOnStsWdr": 0,
		"irOnValBrightness": 50, "irOnValContrast": 50, "irOnValDenoise": 50,
		"irOnValHue": 50, "irOnValSaturation": 50, "irOnValSharpness": 50, "irOnValWdr": 1,
		"mirror": 0, "queryIrLedStatus": 0, "saturation": 50, "sharpness": 50,
		"touchFocusX": 1001, "touchFocusY": 1001, "wdr": 1, "zoomPosition": 0,
	}
}

func (c *Client) process(raw []byte) (forceReconnect bool, err error) {
	var m Envelope
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Printf("failed to parse incoming message: %v (%s)", err, string(raw))
		return false, nil
	}
	fn := m.FunctionName
	log.Printf("recv %s (messageId=%d, responseExpected=%v)", fn, m.MessageID, m.ResponseExpected)

	switch fn {
	case "ubnt_avclient_hello":
		// controller ack. Log the raw payload to see the identity it hands
		// back (real hardware advertises the console UUID as 0x26).
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ubnt_avclient_hello raw payload: %s", raw)
		}
		// The hello reply carries the controller's UUID as `controllerUuid`;
		// real hardware advertises it as discovery TLV 0x26.
		if u, ok := m.Payload["controllerUuid"].(string); ok && u != "" {
			if raw, err := parseUUID(u); err != nil {
				log.Printf("hello: controllerUuid %q is not a valid UUID, ignoring: %v", u, err)
			} else {
				adoptionUUID = raw[:]
				saveAdoptionUUID(adoptionUUIDFilePath, u)
				log.Printf("hello: adopted controllerUuid %s as 0x26 TLV", u)
			}
		}
		return false, nil
	case "ubnt_avclient_paramAgreement":
		// Only sent after accepting our hello, so its arrival IS the
		// adoption-confirmed signal gating discovery's 0x26 TLV.
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ubnt_avclient_paramAgreement raw payload: %s", raw)
		}
		setAdopted(true)
		return false, c.send(c.genResponse("ubnt_avclient_paramAgreement", m.MessageID, map[string]interface{}{
			"authToken": cfg.Token,
			"features":  featureFlags(),
		}))
	case "ubnt_avclient_time":
		return false, c.send(c.genResponse("ubnt_avclient_paramAgreement", m.MessageID, map[string]interface{}{
			"monotonicMs": c.uptime() * 1000,
			"wallMs":      time.Now().UnixMilli(),
			"features":    map[string]interface{}{},
		}))
	case "ubnt_avclient_timeSync":
		return c.handleTimeSync(m)
	case "ResetIspSettings":
		// With mediad, restore vendor defaults on its socket too; off the read
		// loop so a slow socket can't delay the ack.
		go func() {
			if !mediadEnabled() {
				return
			}
			if r, err := mediadCommand("reset"); err != nil {
				log.Printf("mediad ctl: reset: %v", err)
			} else {
				log.Printf("mediad ctl: reset %s", r)
			}
			// mediad is back at its defaults; clear the delta so the
			// controller's next settings object is applied in full.
			mediadDeltaReapply()
		}()
		return false, c.send(c.genResponse("ResetIspSettings", m.MessageID, ispSettingsDefaults()))
	case "ChangeIspSettings":
		return false, c.handleIspSettings(m)
	case "ChangeVideoSettings":
		return false, c.handleVideoSettings(m)
	case "ChangeDeviceSettings":
		// A name set in the Protect app arrives here; persist it instead of
		// echoing the old value, falling back to the current name.
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ChangeDeviceSettings raw payload: %s", raw)
		}
		if newName, ok := m.Payload["name"].(string); ok && newName != "" && newName != getDeviceName() {
			log.Printf("ChangeDeviceSettings: controller renamed device to %q", newName)
			setDeviceName(newName)
		}
		return false, c.send(c.genResponse("ChangeDeviceSettings", m.MessageID, map[string]interface{}{
			"name":     getDeviceName(),
			"timezone": "GMT0",
		}))
	case "ChangeOsdSettings":
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ChangeOsdSettings raw payload: %s", raw)
		}
		// Forward to mediad's burned-in overlay (osdControlMap); off the read
		// loop so a stalled mediad can't delay the controller's ack.
		go mediadApplyOsdSettings(m.Payload)

		return false, c.send(c.genResponse("ChangeOsdSettings", m.MessageID,
			osdSettingsResponse(m.Payload)))
	case "NetworkStatus":
		payload := map[string]interface{}{
			"connectionState": 2, "connectionStateDescription": "CONNECTED",
			"defaultInterface": "wlan0", "ipAddress": cfg.IP, "mode": "dhcp",
		}
		if ws := readWifiStatus(); ws != nil {
			payload["linkSpeedMbps"] = ws.linkSpeedMbps
			payload["channel"] = ws.channel
			payload["essid"] = ws.essid
			payload["frequency"] = ws.frequencyMHz
			payload["signalLevel"] = ws.signalLevelDBm
			payload["bssid"] = ws.bssid
		}
		return false, c.send(c.genResponse("NetworkStatus", m.MessageID, payload))
	case "GetCurrentPosition":
		return false, c.handleGetCurrentPosition(m)
	case "ContinuousMove":
		return false, c.handleContinuousMove(m)
	case "RelativePosition":
		return false, c.handleRelativePosition(m)
	case "Preset":
		return false, c.handlePtzPreset(m)
	case "Center":
		return false, c.handlePtzCenter(m)
	case "Patrol":
		return false, c.handlePtzPatrol(m)
	case "Zoom":
		// No optical zoom on this hardware -- ack only; digital zoom
		// (dZoomScale) is a separate ChangeIspSettings field, not this.
		ptzLogPayload("Zoom", m.Payload)
		if m.ResponseExpected {
			return false, c.send(c.genResponse("Zoom", m.MessageID, nil))
		}
		return false, nil
	case "PanTiltReset", "EnablePtzControl", "DisablePtzControl":
		// PanTiltReset has no ipc_cmd equivalent; Enable/DisablePtzControl look
		// like session-bracketing markers. Ack only.
		ptzLogPayload(fn, m.Payload)
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "ChangePTZAutoTrackSettings":
		// Autotracking needs onboard object tracking this camera lacks --
		// ack only, no state kept.
		ptzLogPayload(fn, m.Payload)
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "AnalyticsTest":
		return false, c.send(c.genResponse("AnalyticsTest", m.MessageID, nil))
	case "ChangeSoundLedSettings":
		return false, c.send(c.genResponse(fn, m.MessageID, c.handleSoundLedSettings(m)))
	case "ChangeAnalyticsSettings":
		return false, c.send(c.genResponse("ChangeAnalyticsSettings", m.MessageID, m.Payload))
	case "UpdateUsernamePassword":
		c.handleUpdateUsernamePassword(m)
		return false, c.send(c.genResponse("UpdateUsernamePassword", m.MessageID, nil))
	case "ChangeTalkbackSettings":
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ChangeTalkbackSettings raw payload: %s", raw)
		}
		if m.ResponseExpected {
			// Accept the controller's requested settings instead of our
			// defaults; answering a different transport was a negotiation mismatch.
			resp := talkbackSettingsDefaults()
			for k, v := range m.Payload {
				resp[k] = v
			}
			return false, c.send(c.genResponse(fn, m.MessageID, resp))
		}
		return false, nil
	case "ChangeBrightnessSettings":
		// Legacy separate brightness message (schema uncaptured), run through
		// the same mapping; it is PARTIAL, so it must not end the delta seed.
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ChangeBrightnessSettings raw payload: %s", raw)
		}
		go mediadApplyIspSettings(m.Payload, false)
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "ChangeSmartDetectSettings", "ChangeSmartMotionSettings", "ChangeClarityZones",
		"ChangeAudioEventsSettings", "ChangeInterfaceSettings",
		"AudioAgentChangeTuning", "SmartMotionTest",
		"SendWeatherUpdate", "DisableLogging", "StartService":
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "UpdateFirmwareRequest":
		// Not a real upgrade: fetch the image's version string and adopt it. Every
		// reachable image is encrypted, so ignore rather than reconnect forever.
		if uri, ok := m.Payload["uri"].(string); ok {
			if v, err := fetchFirmwareVersion(uri); err != nil {
				log.Printf("UpdateFirmwareRequest: failed to read version from %s: %v", uri, err)
			} else if !looksLikeVersionString(v) {
				log.Printf("UpdateFirmwareRequest: parsed version doesn't look valid, ignoring: %q", v)
			} else {
				log.Printf("pretending to upgrade to: %s", v)
				cfg.FWVersion = v
			}
		}
		return false, nil
	case "Reboot":
		return true, nil
	case "GetRequest":
		// Must not block: process() runs inline in ReadMessage, and gorilla only
		// handles ping/pong there -- a hang would trip the missed-pong timeout.
		go c.handleGetRequest(m)
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "ResetToDefaults":
		// Sent when an admin removes this camera from Protect: performReset puts
		// us in awaiting-manage and deletes identity; runs in the background.
		go c.performReset()
		return false, nil
	default:
		// Log the raw payload for anything unhandled so an unrecognized
		// functionName's contents are visible on first arrival.
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("%s raw payload (unhandled): %s", fn, raw)
		}
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	}
}

// runCpldCtl shells out to cpld_ctl for the confirmed /dev/cpld_periph IR-cut
// filter and IR LED ioctls; failures are logged, not returned.
func runCpldCtl(args ...string) {
	if err := exec.Command(unifiPrefix+"/bin/cpld_ctl", args...).Run(); err != nil {
		log.Printf("cpld_ctl %v failed: %v", args, err)
	}
}

// setStatusLed turns the camera's status LED on/off via ipc_cmd -l. This is
// the status LED, not the IR illuminator (that is cpld_ctl's led/ircut).
func setStatusLed(on bool) {
	arg := "off"
	if on {
		arg = "on"
	}
	if err := exec.Command(unifiPrefix+"/bin/ipc_cmd", "-l", arg).Run(); err != nil {
		log.Printf("ipc_cmd -l %s failed: %v", arg, err)
	}
}

// runStatusLedBlinker blinks the status LED (1 s on/off) while not adopted,
// then leaves it on steady. Real hardware signals provisioning state this way.
func runStatusLedBlinker() {
	setStatusLed(true)
	on := true
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		if isAdopted.Load() {
			setStatusLed(true)
			log.Printf("status LED: adopted, steady on")
			return
		}
		on = !on
		setStatusLed(on)
	}
}

// handleIspSettings wires irLedMode/irLedLevel to cpld_ctl: ioctls 0x7015/0x7016
// for the IR-cut filter and 0x7013 for the LED array; "auto" may be lux (lux.go).
func (c *Client) handleIspSettings(m Envelope) error {
	if raw, err := json.Marshal(m.Payload); err == nil {
		log.Printf("ChangeIspSettings raw payload: %s", raw)
	}

	// When mediad is the producer it owns night vision via nightvision/night_lux/
	// ir_led, so skip the local cpld_ctl/lux path; with stock rmm keep it.
	if mediadEnabled() {
		setIcrLuxMode(false, 0, false)
	} else if mode, ok := m.Payload["irLedMode"].(string); ok {
		level, _ := m.Payload["irLedLevel"].(float64)
		switch mode {
		case "manual":
			// Manual takes precedence over any lux-threshold state -- disable
			// the poller so it doesn't fight this explicit setting.
			setIcrLuxMode(false, 0, false)
			if level > 0 {
				runCpldCtl("ircut", "out")
				runCpldCtl("led", "100")
				log.Printf("manual night vision ON (irLedLevel=%.0f): ircut out + led 100", level)
			} else {
				runCpldCtl("led", "0")
				runCpldCtl("ircut", "in")
				log.Printf("manual night vision OFF (irLedLevel=%.0f): led 0 + ircut in", level)
			}
		case "auto":
			// Two UI states arrive with irLedMode=="auto": plain Auto
			// (sensitivity) and Custom with a 1-30 lux threshold (see above).
			switchMode, _ := m.Payload["icrSwitchMode"].(string)
			if switchMode == "lux" {
				slider, _ := m.Payload["icrCustomValue"].(float64)
				extIr, _ := m.Payload["enableExternalIr"].(float64)
				setIcrLuxMode(true, int(slider), extIr != 0)
			} else {
				setIcrLuxMode(false, 0, false)
				log.Printf("irLedMode=auto, icrSwitchMode=%q: leaving IR to native rmm auto day/night switching", switchMode)
			}
		}
	}

	// Forward picture + night-vision controls to mediad when the advanced path
	// is live; off the read loop so a stalled mediad can't delay the ack.
	go mediadApplyIspSettings(m.Payload, true)

	// Echo the controller's real values back, layered under
	// ispSettingsDefaults() so uncovered fields get a sane fallback.
	resp := ispSettingsDefaults()
	for k, v := range m.Payload {
		resp[k] = v
	}
	return c.send(c.genResponse("ChangeIspSettings", m.MessageID, resp))
}

// handleSoundLedSettings wires the real status LED (ipc_cmd -l) to the
// controller's ledFaceEnabled (0/1) field, and the speaker/talkback gate to
// speakerEnabled. The reply must echo what the controller sent: hardcoding
// speakerEnabled=1 made Protect believe talkback was still on after the user
// disabled it, so the toggle could never come back off.
func (c *Client) handleSoundLedSettings(m Envelope) map[string]interface{} {
	if raw, err := json.Marshal(m.Payload); err == nil {
		log.Printf("ChangeSoundLedSettings raw payload: %s", raw)
	}

	ledOn := true
	if v, ok := m.Payload["ledFaceEnabled"]; ok {
		switch t := v.(type) {
		case float64:
			ledOn = t != 0
		case bool:
			ledOn = t
		}
		arg := "off"
		if ledOn {
			arg = "on"
		}
		if err := exec.Command(unifiPrefix+"/bin/ipc_cmd", "-l", arg).Run(); err != nil {
			log.Printf("ipc_cmd -l %s failed: %v", arg, err)
		} else {
			log.Printf("ipc_cmd -l %s (status LED)", arg)
		}
	}

	speakerOn := 1
	if v, ok := m.Payload["speakerEnabled"]; ok {
		speakerOn = 0
		switch t := v.(type) {
		case float64:
			if t != 0 {
				speakerOn = 1
			}
		case bool:
			if t {
				speakerOn = 1
			}
		}
	}
	// NOTE: the controller has never been observed sending speakerEnabled=0
	// (checked across full sessions on y623), so this only echoes its value -
	// it does not gate the talkback receiver. Talkback enable/disable is a
	// controller<->browser negotiation plus the aecTalkbackSwitch feature flag;
	// the camera learns of audio only from the packets on UDP :7004.

	ledVal := 0
	if ledOn {
		ledVal = 1
	}
	return map[string]interface{}{
		"ledFaceAlwaysOnWhenManaged": 1, "ledFaceEnabled": ledVal, "speakerEnabled": speakerOn,
		"speakerVolume": 100, "systemSoundsEnabled": 1, "userLedBlinkPeriodMs": 0,
		"userLedColorFg": "blue", "userLedOnNoff": ledVal,
	}
}

// handleVideoSettings responds to ChangeVideoSettings. The client is the
// authority: always return a complete hardcoded video1/2/3/mjpg schema.
func (c *Client) handleVideoSettings(m Envelope) error {
	if raw, err := json.Marshal(m.Payload); err == nil {
		log.Printf("ChangeVideoSettings raw payload: %s", raw)
	}

	// {audio:{bitRate,volume}}: volume==0 mutes. Real hardware sets ADC gain 0
	// so audio keeps flowing silently (FlvPush does MUTE); absent block keeps last.
	// The reply must echo the controller's own values (a real G3 answers with
	// bitRate 64000 / quality 1); answering 32000/0 is what made the controller's
	// "Auto" quality select no audio track. See the G3 streamer configs.
	audioVolume := 100
	audioBitRate := 64000
	c.micMu.Lock()
	audioVolume = c.micVolume
	if c.micBitRate > 0 {
		audioBitRate = c.micBitRate
	}
	hasAudio := false
	if audio, ok := m.Payload["audio"].(map[string]interface{}); ok {
		hasAudio = true
		if v, ok := audio["volume"].(float64); ok {
			audioVolume = int(v)
		}
		if br, ok := audio["bitRate"].(float64); ok && br > 0 {
			c.micBitRate = int(br)
		}
		if en, ok := audio["enabled"].(bool); ok && !en {
			audioVolume = 0
		}
		c.micVolume = audioVolume
	}
	c.micMu.Unlock()
	if hasAudio {
		c.setMicLevel(micLevelFromVolume(audioVolume))
	}

	vidDst := map[string]string{
		"video1": "file:///dev/null",
		"video2": "file:///dev/null",
		"video3": "file:///dev/null",
	}

	// The HIGH (video1) resolution comes from the shipped per-model table, not
	// from this binary.
	video1Width, video1Height := readHighResolution()

	// A model_table high_bitrate pins this model's HIGH channel: the encoder
	// target and the declared bitrates below all use it instead of the
	// controller's requested value (0 = follow the controller).
	pinnedHighBps := modelHighBitrate()
	declaredHighBps := 2000000
	if pinnedHighBps > 0 {
		declaredHighBps = pinnedHighBps
	}

	if video, ok := m.Payload["video"].(map[string]interface{}); ok {
		// Shutter (Video Mode) and HIGH bitrate -> mediad, delta-gated so the
		// controller's periodic resend doesn't re-issue them.
		var vidControls []mediadCtl
		vidControls = append(vidControls, shutterControls(video)...)
		vidBps := pinnedHighBps
		if vidBps == 0 {
			if v1, ok := video["video1"].(map[string]interface{}); ok {
				vidBps = videoStreamBitrate(v1)
			}
		}
		if vidBps > 0 {
			vidControls = append(vidControls, mediadCtl{key: "bitrate", value: clampBitrate(vidBps)})
		}
		go mediadApplyControls(vidControls, mediadVidDelta, true)
		for _, key := range []string{"video1", "video2", "video3"} {
			v, ok := video[key].(map[string]interface{})
			if !ok {
				continue
			}
			ser, ok := v["avSerializer"].(map[string]interface{})
			if !ok {
				continue
			}
			dests, ok := ser["destinations"].([]interface{})
			if !ok || len(dests) == 0 {
				continue
			}
			destStr, ok := dests[0].(string)
			if !ok {
				continue
			}
			vidDst[key] = destStr
			u, err := url.Parse(destStr)
			if err != nil || u.Host == "" {
				if key == "video1" {
					c.videoMu.Lock()
					c.activeVideo1Host, c.activeVideo1StreamName = "", ""
					c.videoMu.Unlock()
					stopVideoStream("high")
				} else if key == "video2" {
					c.videoMu.Lock()
					c.activeVideo2Host, c.activeVideo2StreamName = "", ""
					c.videoMu.Unlock()
					stopVideoStream("low")
				} else if key == "video3" {
					c.videoMu.Lock()
					c.activeVideo3Host, c.activeVideo3StreamName = "", ""
					c.videoMu.Unlock()
					stopVideoStream("medium")
				}
				continue
			}
			streamName := ""
			if params, ok := ser["parameters"].(map[string]interface{}); ok {
				if sn, ok := params["streamName"].(string); ok {
					streamName = sn
				}
			}
			c.streamsMu.Lock()
			if c.streams == nil {
				c.streams = map[string]string{}
			}
			c.streams[key] = streamName
			c.streamsMu.Unlock()
			// video3 aliases the SAME real LOW frames as video2; streamName is
			// the controller-issued FLV onMetaData token (reconnect only if changed).
			c.videoMu.Lock()
			if key == "video1" {
				if u.Host != c.activeVideo1Host || streamName != c.activeVideo1StreamName {
					c.activeVideo1Host = u.Host
					c.activeVideo1StreamName = streamName
					c.videoMu.Unlock()
					if !startVideoStream(u.Host, streamName, "high") {
						c.videoMu.Lock()
						c.activeVideo1Host, c.activeVideo1StreamName = "", ""
						c.videoMu.Unlock()
					}
				} else {
					c.videoMu.Unlock()
				}
			} else if key == "video2" {
				if u.Host != c.activeVideo2Host || streamName != c.activeVideo2StreamName {
					c.activeVideo2Host = u.Host
					c.activeVideo2StreamName = streamName
					c.videoMu.Unlock()
					if !startVideoStream(u.Host, streamName, "low") {
						c.videoMu.Lock()
						c.activeVideo2Host, c.activeVideo2StreamName = "", ""
						c.videoMu.Unlock()
					}
				} else {
					c.videoMu.Unlock()
				}
			} else if key == "video3" {
				if u.Host != c.activeVideo3Host || streamName != c.activeVideo3StreamName {
					c.activeVideo3Host = u.Host
					c.activeVideo3StreamName = streamName
					c.videoMu.Unlock()
					if !startVideoStream(u.Host, streamName, "medium") {
						c.videoMu.Lock()
						c.activeVideo3Host, c.activeVideo3StreamName = "", ""
						c.videoMu.Unlock()
					}
				} else {
					c.videoMu.Unlock()
				}
			} else {
				c.videoMu.Unlock()
			}
		}
	}

	streamParams := func(key string) interface{} {
		c.streamsMu.Lock()
		defer c.streamsMu.Unlock()
		sn, ok := c.streams[key]
		if !ok {
			return nil
		}
		return map[string]interface{}{
			"audioId": nil, "streamName": sn, "suppressAudio": nil,
			"suppressVideo": nil, "videoId": nil,
		}
	}

	payload := map[string]interface{}{
		"firmwarePath": "/lib/firmware/",
		// FlvPush muxes real mic audio into the FLV output, so audio is
		// enabled. sampleRate/channels match the real encoder's ADTS headers.
		// The field values mirror a real G3's streamer config (quality 1,
		// bitRate from the controller); a quality of 0 made the controller's
		// "Auto" quality pick no audio track. See ubnt_streamer_sysid_a590.json.
		"audio": map[string]interface{}{
			"bitRate": audioBitRate, "channels": 1, "description": "audio track",
			"enableTemporalNoiseShaping": false, "enabled": true, "mode": 0,
			"quality": 1, "sampleRate": 16000, "type": "aac", "volume": audioVolume,
		},
		"video": map[string]interface{}{
			"enableHrd": false, "hdrMode": 0, "lowDelay": false,
			"videoMode": "default", "vinFps": encoderFps,
			"mjpg": map[string]interface{}{
				"avSerializer": map[string]interface{}{
					"destinations": []string{"file:///tmp/snap.jpeg", "file:///tmp/snap_av.jpg"},
					"parameters": map[string]interface{}{
						"audioId": 1000, "enableTimestampsOverlapAvoidance": false,
						"suppressAudio": true, "suppressVideo": false, "videoId": 1001,
					},
					"type": "mjpg",
				},
				"bitRateCbrAvg": 500000, "bitRateVbrMax": 500000, "bitRateVbrMin": nil,
				"description": "JPEG pictures", "enabled": true, "fps": 5,
				"height": 720, "isCbr": false, "maxFps": 5,
				"minClientAdaptiveBitRate": 0, "minMotionAdaptiveBitRate": 0, "nMultiplier": nil,
				"name": "mjpg", "quality": 80, "sourceId": 3, "streamId": 8, "streamOrdinal": 3,
				"type": "mjpg", "validBitrateRangeMax": 6000000, "validBitrateRangeMin": 32000,
				"width": 1280,
			},
			// Real hardware resolution -- per-model (see video1Width/Height),
			// not a generic catalog default.
			"video1": map[string]interface{}{
				"M": 1, "N": encoderFps,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video1"]},
					"parameters":   streamParams("video1"),
					"type":         "extendedFlv",
				},
				"bitRateCbrAvg": declaredHighBps, "bitRateVbrMax": 2800000, "bitRateVbrMin": 48000,
				"description": "Hi quality video track", "enabled": true, "fps": encoderFps,
				"gopModel": 0, "height": video1Height, "horizontalFlip": false,
				"isCbr": false, "maxFps": encoderFps, "minClientAdaptiveBitRate": 0,
				"minMotionAdaptiveBitRate": 0, "nMultiplier": 6, "name": "video1",
				"sourceId": 0, "streamId": 1, "streamOrdinal": 0, "type": "h264",
				"validBitrateRangeMax": 2800000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{encoderFps},
				"verticalFlip":   false,
				"width":          video1Width,
			},
			"video2": map[string]interface{}{
				"M": 1, "N": encoderFps,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video2"]},
					"parameters":   streamParams("video2"),
					"type":         "extendedFlv",
				},
				// Real LOW resolution (640x360) -- must match the encoder.
				"bitRateCbrAvg": 500000, "bitRateVbrMax": 750000, "bitRateVbrMin": 48000,
				"currentVbrBitrate": 500000, "description": "Low quality video track",
				"enabled": true, "fps": encoderFps, "gopModel": 0, "height": 360,
				"horizontalFlip": false, "isCbr": false, "maxFps": encoderFps,
				"minClientAdaptiveBitRate": 0, "minMotionAdaptiveBitRate": 0, "nMultiplier": 6,
				"name": "video2", "sourceId": 1, "streamId": 2, "streamOrdinal": 1, "type": "h264",
				"validBitrateRangeMax": 750000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{encoderFps},
				"verticalFlip":   false,
				"width":          640,
			},
			"video3": map[string]interface{}{
				"M": 1, "N": encoderFps,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video3"]},
					"parameters":   streamParams("video3"),
					"type":         "extendedFlv",
				},
				// video3 carries the real LOW stream (see activeVideo3*).
				"bitRateCbrAvg": 500000, "bitRateVbrMax": 750000, "bitRateVbrMin": 48000,
				"currentVbrBitrate": 500000, "description": "Medium quality video track",
				"enabled": true, "fps": encoderFps, "gopModel": 0, "height": 360,
				"horizontalFlip": false, "isCbr": false, "maxFps": encoderFps,
				"minClientAdaptiveBitRate": 0, "minMotionAdaptiveBitRate": 0, "nMultiplier": 6,
				"name": "video3", "sourceId": 2, "streamId": 4, "streamOrdinal": 2, "type": "h264",
				"validBitrateRangeMax": 750000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{encoderFps},
				"verticalFlip":   false,
				"width":          640,
			},
		},
	}
	return c.send(c.genResponse("ChangeVideoSettings", m.MessageID, payload))
}

// handleGetRequest answers "GetRequest": grabs a JPEG via imggrabber and HTTP
// POSTs it with the WSS mTLS cert; imggrabber blocks until the next IDR.
func (c *Client) handleGetRequest(m Envelope) {
	what, _ := m.Payload["what"].(string)
	uri, _ := m.Payload["uri"].(string)
	if uri == "" {
		log.Printf("GetRequest[%s]: no uri in payload, nothing to upload", what)
		return
	}

	res := "high"
	if q, _ := m.Payload["quality"].(string); q == "low" {
		res = "low"
	}

	model := readHardwareModel()

	timeout := snapshotTimeout(m.Payload)

	// Serialize grabs: each imggrabber costs ~9MB RSS and fights rmm for the
	// shared sensor/ISP on a 60MB board.
	c.snapshotGrabMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, unifiPrefix+"/bin/imggrabber", "-m", model, "-r", res)
	cmd.Stderr = &stderr
	jpeg, err := cmd.Output()
	cancel()
	c.snapshotGrabMu.Unlock()

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("GetRequest[%s]: imggrabber timed out after %s", what, timeout)
		} else {
			log.Printf("GetRequest[%s]: imggrabber failed: %v (stderr: %s)", what, err, strings.TrimSpace(stderr.String()))
		}
		cached, age, ok := c.cachedSnapshot(res)
		if !ok {
			return
		}
		log.Printf("GetRequest[%s]: falling back to cached %s snapshot (%d bytes, age %s)", what, res, len(cached), age.Round(time.Second))
		jpeg = cached
	} else {
		c.storeSnapshot(res, jpeg)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("payload", "payload.jpg")
	if err != nil {
		log.Printf("GetRequest[%s]: building multipart body failed: %v", what, err)
		return
	}
	if _, err := fw.Write(jpeg); err != nil {
		log.Printf("GetRequest[%s]: writing jpeg into multipart body failed: %v", what, err)
		return
	}
	// Echo back any extra form fields the controller asked for (never
	// observed live, but cheap to support).
	if extra, ok := m.Payload["formFields"].(map[string]interface{}); ok {
		for k, v := range extra {
			if s, ok := v.(string); ok {
				mw.WriteField(k, s)
			}
		}
	}
	if err := mw.Close(); err != nil {
		log.Printf("GetRequest[%s]: closing multipart body failed: %v", what, err)
		return
	}

	req, err := http.NewRequest("POST", uri, &body)
	if err != nil {
		log.Printf("GetRequest[%s]: building upload request failed: %v", what, err)
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("GetRequest[%s]: upload to %s failed: %v", what, uri, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("GetRequest[%s]: uploaded %d-byte snapshot to %s, status=%s", what, len(jpeg), uri, resp.Status)
}

const (
	// fallback when the controller sends no usable timeoutMs -- comfortably
	// above the ~9s HIGH GOP.
	defaultSnapshotTimeout = 25 * time.Second
	minSnapshotTimeout     = 10 * time.Second
	// headroom below the controller's granted deadline (60s observed) so we
	// always log the outcome before it gives up.
	maxSnapshotTimeout = 55 * time.Second
	// a fallback older than this is worse than nothing -- drop it so we fail
	// loudly instead of uploading a misleadingly old frame.
	snapshotCacheTTL = 5 * time.Minute
)

// snapshotTimeout honors the controller's timeoutMs (60000 live) clamped to a
// sane range; waiting for the next IDR is normal, so it is deliberately generous.
func snapshotTimeout(payload map[string]interface{}) time.Duration {
	t := defaultSnapshotTimeout
	if ms, ok := payload["timeoutMs"].(float64); ok && ms > 0 {
		t = time.Duration(ms) * time.Millisecond
	}
	if t < minSnapshotTimeout {
		t = minSnapshotTimeout
	}
	if t > maxSnapshotTimeout {
		t = maxSnapshotTimeout
	}
	return t
}

// storeSnapshot caches the most recent good JPEG for a resolution so a
// later grab that can't win a keyframe race still has something to send.
func (c *Client) storeSnapshot(res string, jpeg []byte) {
	cp := make([]byte, len(jpeg))
	copy(cp, jpeg)
	c.snapshotMu.Lock()
	c.snapshotJPEG[res] = cp
	c.snapshotAt[res] = time.Now()
	c.snapshotMu.Unlock()
}

// cachedSnapshot returns the last good JPEG for res and its age, or ok=false
// if none exists or it's older than snapshotCacheTTL.
func (c *Client) cachedSnapshot(res string) ([]byte, time.Duration, bool) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	jpeg, ok := c.snapshotJPEG[res]
	if !ok {
		return nil, 0, false
	}
	age := time.Since(c.snapshotAt[res])
	if age > snapshotCacheTTL {
		return nil, age, false
	}
	return jpeg, age, true
}

// looksLikeVersionString sanity-checks bytes pulled from a firmware image; real
// images may be encrypted and have no plain version there.
func looksLikeVersionString(v string) bool {
	if len(v) < 3 || len(v) > 64 {
		return false
	}
	// Real version strings start with a letter and contain dots; garbage from an
	// encrypted image (a hex digest) won't match both.
	first := v[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')) {
		return false
	}
	if !strings.Contains(v, ".") {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// fetchFirmwareVersion reads the null-padded version at bytes 4-53 (Range 0-100)
// of a firmware image, matching unifi-cam-proxy's process_upgrade().
func fetchFirmwareVersion(uri string) (string, error) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes=0-100")
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed controller cert
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	buf := make([]byte, 54)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return "", err
	}
	version := ""
	for _, b := range buf[4:54] {
		if b != 0 {
			version += string(b)
		}
	}
	return version, nil
}

// FlvPush control FIFO of the deployed unifi_flv_bridge (work/flv_bridge/); an
// earlier ffmpeg-spawning relay exhausted this 60MB device's RAM.
const flvPushFifo = "/tmp/unifi_flv_bridge_ctl"

var (
	videoStreamMu sync.Mutex
	// The FIFO reader only holds the pipe open briefly between writers, so
	// per-command opens raced it (ENXIO); hold one writer open for the lifetime.
	flvPushFifoFile *os.File
	// currentMicLevel mirrors the effective mic level so startVideoStream can
	// re-assert it on a fresh CONNECT; defaults to full (unmuted).
	currentMicLevel = 100
)

// channel is "high", "low", or "medium" -- FlvPush tracks each destination
// separately, so they can be connected/reconnected independently.
// startVideoStream reports whether FlvPush got the CONNECT. On false the
// caller forgets the destination so the controller's next re-send retries
// (it re-sends ~every 10 s); otherwise a CONNECT lost while the bridge was
// restarting left that channel dead, as every re-send looked like a duplicate.
func startVideoStream(dest, streamName, channel string) bool {
	videoStreamMu.Lock()
	defer videoStreamMu.Unlock()
	cmd := fmt.Sprintf("CONNECT %s %s %s\n", dest, streamName, channel)
	if err := writeFlvPushFifoLocked(cmd); err != nil {
		log.Printf("startVideoStream[%s]: failed to write FlvPush FIFO: %v", channel, err)
		return false
	}
	// Re-assert the mic state on every CONNECT: FlvPush's flag resets if the
	// bridge restarts, and the encoder may re-init the mixer.
	arg := "off"
	if currentMicLevel <= 0 {
		arg = "on"
	}
	if err := writeFlvPushFifoLocked(fmt.Sprintf("MUTE %s\n", arg)); err != nil {
		log.Printf("startVideoStream[%s]: failed to re-assert mic mute: %v", channel, err)
	}
	applyMicGain(currentMicLevel)
	log.Printf("startVideoStream[%s]: told FlvPush to push to %s, streamName=%s", channel, dest, streamName)
	return true
}

func stopVideoStream(channel string) {
	videoStreamMu.Lock()
	defer videoStreamMu.Unlock()
	if err := writeFlvPushFifoLocked(fmt.Sprintf("DISCONNECT %s\n", channel)); err != nil {
		log.Printf("stopVideoStream[%s]: failed to write FlvPush FIFO: %v", channel, err)
	}
}

// micLevelFromVolume maps the controller's volume to 0..100 (0 = mute).
// Protect's slider floors at 1, so its minimum is treated as mute.
func micLevelFromVolume(volume int) int {
	if volume <= 1 {
		return 0
	}
	if volume > 100 {
		return 100
	}
	return volume
}

// setMicLevel applies an effective level (0..100): FlvPush substitutes silent
// audio at level 0 and mixer_set scales the codec gain; no-op if unchanged.
func (c *Client) setMicLevel(level int) {
	c.micMu.Lock()
	defer c.micMu.Unlock()
	if c.micLevelSet && c.micLevel == level {
		return
	}
	c.micLevel = level
	c.micLevelSet = true

	videoStreamMu.Lock()
	defer videoStreamMu.Unlock()
	currentMicLevel = level
	arg := "off"
	if level <= 0 {
		arg = "on"
	}
	if err := writeFlvPushFifoLocked(fmt.Sprintf("MUTE %s\n", arg)); err != nil {
		log.Printf("setMicLevel: failed to write FlvPush FIFO: %v", err)
	}
	applyMicGain(level)
	state := "unmuted"
	if level <= 0 {
		state = "muted"
	}
	log.Printf("setMicLevel: mic %s, level %d (controller audio volume)", state, level)
}

// applyMicGain writes the level to the codec gain element via bin/mixer_set;
// best-effort (failure logged). Caller must hold videoStreamMu.
func applyMicGain(level int) {
	out, err := exec.Command(unifiPrefix+"/bin/mixer_set",
		micGainCard, micGainControl, strconv.Itoa(level)).CombinedOutput()
	if err != nil {
		log.Printf("applyMicGain: mixer_set failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("applyMicGain: %s", strings.TrimSpace(string(out)))
}

// Caller must hold videoStreamMu.
func writeFlvPushFifoLocked(line string) error {
	if flvPushFifoFile == nil {
		f, err := openFlvPushFifo()
		if err != nil {
			return err
		}
		flvPushFifoFile = f
	}
	if _, err := flvPushFifoFile.WriteString(line); err != nil {
		// The reader may have gone away (unifi_flv_bridge restarted); drop our
		// stale handle and retry once with a fresh open.
		flvPushFifoFile.Close()
		flvPushFifoFile = nil
		f, ferr := openFlvPushFifo()
		if ferr != nil {
			return ferr
		}
		flvPushFifoFile = f
		_, err = flvPushFifoFile.WriteString(line)
		return err
	}
	return nil
}

func openFlvPushFifo() (*os.File, error) {
	var f *os.File
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err = os.OpenFile(flvPushFifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open %s: %w (is unifi_flv_bridge running?)", flvPushFifo, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func run(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load client cert: %w", err)
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true, // controller uses a self-issued cert; we don't validate it (matches unifi-cam-proxy)
		},
		Subprotocols:     []string{"secure_transfer"},
		HandshakeTimeout: 15 * time.Second,
	}

	u := fmt.Sprintf("wss://%s:%d/camera/1.0/ws", cfg.Host, cfg.Port)
	if cfg.Token != "" {
		u += "?token=" + cfg.Token
	}
	// Per-connection GUID: real cameras mint a fresh x-guid each time they
	// dial, unlike device-id which stays constant across reconnects/reboots.
	connGUID, _, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("generate x-guid: %w", err)
	}
	headers := http.Header{}
	headers.Set("camera-mac", cfg.MAC)
	headers.Set("camera-ip", cfg.IP)
	// camera-model is the hex system id (e.g. "0xa590"), NOT the model name
	// string (see cfg.SysID). The JSON hello's "model" field is separate.
	headers.Set("camera-model", fmt.Sprintf("0x%x", cfg.SysID))
	headers.Set("camera-firmware", cfg.FWVersion)
	headers.Set("device-id", deviceIDStr)
	headers.Set("x-guid", connGUID)
	// Use the persisted isAdopted flag (set on paramAgreement), NOT cfg.Token:
	// a manage-API push populates the token and it never goes back to empty.
	headers.Set("adopted", fmt.Sprintf("%t", isAdopted.Load()))

	log.Printf("connecting to %s", u)
	conn, resp, err := dialer.DialContext(ctx, u, headers)
	if err != nil {
		if resp != nil {
			log.Printf("handshake failed: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{cert},
				InsecureSkipVerify: true, // same as the WSS connection -- controller uses a self-issued cert
			},
		},
		Timeout: 30 * time.Second,
	}
	client := &Client{
		ws: conn, startTS: time.Now(), httpClient: httpClient,
		snapshotJPEG: map[string][]byte{}, snapshotAt: map[string]time.Time{},
		micVolume: 100, micBitRate: 64000,
	}

	// Seed the mediad delta on every fresh WSS connection: the controller's first
	// settings object is full and must be recorded, not re-applied (a burst risk).
	resetMediadDelta()

	// Real hardware corrects its clock before introducing itself (timesync.go);
	// without it the controller rejects every pushed frame on a multi-year diff.
	if err := client.sendTimeSync(); err != nil {
		log.Printf("initial timeSync send failed: %v", err)
	}
	if err := client.initAdoption(); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	log.Printf("hello sent, waiting for messages...")

	timeSyncDone := make(chan struct{})
	defer close(timeSyncDone)
	go client.startTimeSyncLoop(timeSyncDone)

	motionDone := make(chan struct{})
	defer close(motionDone)
	go client.startMotionWatcher(motionDone)

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
			continue
		}
		forceReconnect, err := client.process(data)
		if err != nil {
			log.Printf("error processing message: %v", err)
		}
		if forceReconnect {
			return fmt.Errorf("controller requested reconnect/reboot")
		}
	}
}

// runUpdatesConnection is real hardware's second WSS connection (ubnt_reportd):
// subprotocol "logs1", bare Camera-MAC, no payload after connecting.
func runUpdatesConnection(ctx context.Context) {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		c := cfgSnapshot()
		if err := dialUpdatesConnectionOnce(ctx, c); err != nil {
			log.Printf("updates connection: %v, reconnecting in %s", err, backoff)
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func dialUpdatesConnectionOnce(ctx context.Context, c Config) error {
	// A client cert is REQUIRED here -- TLS fails ("certificate required")
	// without one; real hardware shares the cert with ubnt_reportd.
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return fmt.Errorf("load client cert: %w", err)
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true, // matches run()'s main connection -- controller uses a self-issued cert
		},
		HandshakeTimeout: 15 * time.Second,
		// ubnt_reportd negotiates subprotocol "logs1" (live heap read), not the
		// main connection's "secure_transfer".
		Subprotocols: []string{"logs1"},
	}

	u := fmt.Sprintf("wss://%s:%d/camera/1.0/ws", c.Host, c.Port)
	headers := http.Header{}
	headers.Set("Camera-MAC", c.MAC)
	// Real ubnt_reportd's Host header is bare (no port); override gorilla's
	// host:port default in case connection classification keys off it.
	headers.Set("Host", c.Host)

	log.Printf("updates connection: connecting to %s", u)
	conn, resp, err := dialer.DialContext(ctx, u, headers)
	if err != nil {
		if resp != nil {
			log.Printf("updates connection: handshake failed: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Printf("updates connection: connected")

	// No application payload (matching ubnt_reportd), so keepalive is needed or
	// the controller stops treating it as a live companion.
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil,
					time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

// detectInterfaceIdentity reads one named interface's real MAC and IPv4 address.
func detectInterfaceIdentity(name string) (mac string, ip string, err error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", "", fmt.Errorf("lookup %s: %w", name, err)
	}
	if len(iface.HardwareAddr) == 0 {
		return "", "", fmt.Errorf("%s has no hardware address", name)
	}
	mac = strings.ToUpper(strings.ReplaceAll(iface.HardwareAddr.String(), ":", ""))

	addrs, err := iface.Addrs()
	if err != nil {
		return "", "", fmt.Errorf("%s addrs: %w", name, err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil && !v4.IsUnspecified() {
			return mac, v4.String(), nil
		}
	}
	return "", "", fmt.Errorf("%s has no IPv4 address", name)
}

// detectNetworkIdentity reads the camera's real MAC and IPv4, preferring eth0
// and falling back to wlan0 (then the cfg placeholders).
func detectNetworkIdentity() (mac string, ip string, err error) {
	if mac, ip, err := detectInterfaceIdentity("eth0"); err == nil {
		return mac, ip, nil
	}
	if mac, ip, err := detectInterfaceIdentity("wlan0"); err == nil {
		return mac, ip, nil
	}
	return "", "", fmt.Errorf("neither wlan0 nor eth0 has a usable IPv4 address")
}

// controllerOnLink reports whether host shares a subnet with this camera:
// off-subnet controllers can't receive discovery broadcast and must be dialed.
func controllerOnLink(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return true
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// cameraConfigFilePath is the legacy per-file home of MODEL/SYSID/FWVERSION,
// still read as a migration fallback for unifi.cfg.
const cameraConfigFilePath = unifiPrefix + "/etc/unifi_client_go.camera-config"

// unifiConfigFilePath is the project's single human-edited config file (plain
// KEY=value); the shell scripts and this client both read it.
const unifiConfigFilePath = unifiPrefix + "/etc/unifi.cfg"

// readUnifiCfg parses unifi.cfg's flat KEY=value lines into a map (keys
// upper-cased); missing/unparsable lines are non-fatal and later duplicates win.
func readUnifiCfg(path string) map[string]string {
	vals := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("unifi.cfg %q exists but couldn't be read: %v", path, err)
		}
		return vals
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.ToUpper(strings.TrimSpace(key))] = strings.TrimSpace(val)
	}
	return vals
}

// isTruthy parses a human-edited config boolean (yes/no plus the usual
// true/false/1/0/on/off aliases); anything else is false.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "on":
		return true
	}
	return false
}

// loadCameraConfigFile reads path (if present) and applies any recognized
// KEY=value lines onto cfg, logging every field it changes.
func loadCameraConfigFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // no file -- not an error, this is an optional override
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			log.Printf("camera-config file %q: malformed line %q, ignoring", path, line)
			continue
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "MODEL":
			log.Printf("camera-config: model %q -> %q", cfg.Model, val)
			cfg.Model = val
		case "SYSID":
			n, perr := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(val), "0x"), 16, 16)
			if perr != nil {
				log.Printf("camera-config file %q: bad SYSID %q, ignoring: %v", path, val, perr)
				continue
			}
			log.Printf("camera-config: sysid 0x%x -> 0x%x", cfg.SysID, n)
			cfg.SysID = uint16(n)
		case "FWVERSION":
			log.Printf("camera-config: fwversion %q -> %q", cfg.FWVersion, val)
			cfg.FWVersion = val
		default:
			log.Printf("camera-config file %q: unrecognized key %q, ignoring", path, key)
		}
	}
}

// defaultInformHostFilePath is runtime state written by applyManagePush so a
// controller adopt-push pin survives restarts; unifi.cfg CONTROLLER outranks it.
const defaultInformHostFilePath = unifiPrefix + "/etc/unifi_client_go.inform-host"

// dhcpInformHostFilePath is the lowest-priority controller source (DHCP option
// 43), kept apart so a lease renewal can't clobber the adopt-push pin.
const dhcpInformHostFilePath = unifiPrefix + "/etc/unifi_client_go.inform-host-dhcp"

// loadInformHostOverride reads a "host" or "host:port" line from path ('#'
// comments and blanks skipped); a missing file gives ok=false.
func loadInformHostOverride(path string) (host string, port int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("inform-host file %q exists but couldn't be read: %v -- ignoring, using compiled-in default", path, err)
		}
		return "", 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if h, p, err := net.SplitHostPort(line); err == nil {
			portNum, perr := strconv.Atoi(p)
			if perr != nil {
				log.Printf("inform-host file %q: bad port in %q: %v -- ignoring line", path, line, perr)
				continue
			}
			return h, portNum, true
		}
		// No ":port" -- treat the whole line as just the host, keep the
		// compiled-in default port.
		return line, 0, true
	}
	return "", 0, false
}

func main() {
	// Wait for an interface to have an address before latching identity: at boot
	// DHCP may not be done, and a one-shot check would latch the bogus placeholder.
	detectedMAC, detectedIP, derr := detectNetworkIdentity()
	if derr != nil {
		log.Printf("network not ready yet (%v); waiting up to 45s for wlan0/eth0 to get an address", derr)
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(1 * time.Second)
			if m, i, err := detectNetworkIdentity(); err == nil {
				detectedMAC, detectedIP, derr = m, i, nil
				break
			}
		}
	}
	if derr != nil {
		log.Printf("warning: could not auto-detect a network identity from wlan0 or eth0 after waiting (%v) -- falling back to placeholder default (%s/%s), which is NOT a real, usable identity", derr, cfg.MAC, cfg.IP)
	} else {
		cfg.MAC = detectedMAC
		cfg.IP = detectedIP
		log.Printf("detected network identity: mac=%s ip=%s", cfg.MAC, cfg.IP)
	}

	// Read project config BEFORE computing flag defaults so explicit flags still
	// win. Controller precedence: unifi.cfg CONTROLLER > inform-host > DHCP.
	loadCameraConfigFile(cameraConfigFilePath)
	unifiCfg := readUnifiCfg(unifiConfigFilePath)
	if v := unifiCfg["MODEL"]; v != "" {
		log.Printf("unifi.cfg: model %q -> %q", cfg.Model, v)
		cfg.Model = v
	}
	if v := unifiCfg["SYSID"]; v != "" {
		if n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(v), "0x"), 16, 16); err != nil {
			log.Printf("unifi.cfg file %q: bad SYSID %q: %v", unifiConfigFilePath, v, err)
		} else {
			log.Printf("unifi.cfg: sysid 0x%x -> 0x%x", cfg.SysID, n)
			cfg.SysID = uint16(n)
		}
	}
	if v := unifiCfg["FWVERSION"]; v != "" {
		log.Printf("unifi.cfg: fwversion %q -> %q", cfg.FWVersion, v)
		cfg.FWVersion = v
	}
	if v := unifiCfg["IS_MEDIAD"]; v != "" {
		cfg.IsMediad = isTruthy(v)
		log.Printf("unifi.cfg: is_mediad %q -> %v", v, cfg.IsMediad)
	}
	cfg.WebUIPort = 80
	if v := unifiCfg["WEBUI_PORT"]; v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 0 && p < 65536 {
			cfg.WebUIPort = p
		} else {
			log.Printf("unifi.cfg: bad WEBUI_PORT %q, using 80", v)
		}
	}
	if v := unifiCfg["MEDIAD_3DNR"]; v != "" {
		cfg.Mediad3DNR = isTruthy(v)
		log.Printf("unifi.cfg: mediad_3dnr %q -> %v", v, cfg.Mediad3DNR)
	}
	if v := unifiCfg["PROTECT_SSH"]; v != "" {
		cfg.ProtectSSH = isTruthy(v)
		log.Printf("unifi.cfg: protect_ssh %q -> %v", v, cfg.ProtectSSH)
	}
	if v := unifiCfg["CONTROLLER"]; v != "" {
		if h, p, err := net.SplitHostPort(v); err == nil {
			cfg.Host = h
			if pn, perr := strconv.Atoi(p); perr == nil {
				cfg.Port = pn
			}
		} else {
			cfg.Host = v
		}
		log.Printf("unifi.cfg CONTROLLER: overriding controller host to %q (port %v)", cfg.Host, cfg.Port)
	} else if h, p, ok := loadInformHostOverride(defaultInformHostFilePath); ok {
		log.Printf("inform-host file %q: overriding default controller host to %q (port default %v)", defaultInformHostFilePath, h, p)
		cfg.Host = h
		if p > 0 {
			cfg.Port = p
		}
	} else if h, p, ok := loadInformHostOverride(dhcpInformHostFilePath); ok {
		log.Printf("dhcp inform-host file %q: overriding default controller host to %q (port default %v) -- no manual override present", dhcpInformHostFilePath, h, p)
		cfg.Host = h
		if p > 0 {
			cfg.Port = p
		}
	}

	token := flag.String("token", "", "adoption token (optional -- omit to connect like a factory-fresh camera awaiting adoption in the UI)")
	host := flag.String("host", cfg.Host, "controller host (default seeded from unifi.cfg CONTROLLER or an inform-host file if present, else compiled-in default)")
	mac := flag.String("mac", cfg.MAC, "camera MAC (no separators) -- auto-detected from wlan0/eth0 by default, override only for testing")
	ip := flag.String("ip", cfg.IP, "camera IP to present -- auto-detected from wlan0/eth0 by default, override only for testing")
	model := flag.String("model", cfg.Model, "model string to present")
	certFile := flag.String("cert", cfg.CertFile, "client cert path (PEM)")
	keyFile := flag.String("key", cfg.KeyFile, "client key path (PEM)")
	noDiscovery := flag.Bool("no-discovery", false, "disable the UDP 10001 discovery responder")
	ptz := flag.Bool("ptz", cfg.HasPTZ, "declare PTZ (pan/tilt/zoom) hardware capability -- only for units with a real motorized pan/tilt base")
	mediad := flag.Bool("mediad", cfg.IsMediad, "enable advanced picture controls via the custom mediad rmm replacement (requires unifi.cfg IS_MEDIAD=yes and a running mediad)")
	guid := flag.String("guid", "ffffffff-ffff-ffff-ffff-ffffffffffff", "device GUID advertised in discovery (default matches an unset/unadopted board.guid)")
	deviceIDFile := flag.String("device-id-file", unifiPrefix+"/etc/unifi_client_go.device-id", "path to persist the stable per-device adoption UUID (device-id header / discovery 0x26 TLV)")
	manageAwaitFile := flag.String("manage-await-file", manageAwaitFilePath, "path used to persist 'awaiting controller adopt push' state across a ResetToDefaults reboot (see manage.go)")
	adoptedStateFile := flag.String("adopted-state-file", adoptedStateFilePath, "path used to persist adoption state across restarts, so discovery reports it correctly from the first reply after any restart (see discovery.go)")
	flag.Parse()

	cfg.Token = *token
	cfg.Host = *host
	cfg.MAC = *mac
	cfg.IP = *ip
	cfg.Model = *model
	cfg.CertFile = *certFile
	cfg.KeyFile = *keyFile
	// PTZ precedence: explicit -ptz flag, then unifi.cfg PTZ=, else the model
	// table -- the single place the capability is decided.
	if flagSet("ptz") {
		cfg.HasPTZ = *ptz
	} else if v := unifiCfg["PTZ"]; v != "" {
		cfg.HasPTZ = isTruthy(v)
	} else {
		def, _ := readModelDef(readHardwareModel())
		cfg.HasPTZ = def.ptz
	}
	cfg.IsMediad = *mediad

	// One-shot visibility of the mediad state when IS_MEDIAD is set; the
	// per-message gate (mediadEnabled) is what actually decides.
	logMediadStatus()

	// Mint a fresh mTLS cert/key now if missing (first boot after a factory
	// reset, or a first-ever install). Must run before run() loads them.
	if err := ensureCertKey(cfg.CertFile, cfg.KeyFile); err != nil {
		log.Fatalf("failed to ensure client cert/key: %v", err)
	}

	deviceIDFilePath = *deviceIDFile
	var deviceIDBytes [16]byte
	var err error
	deviceIDStr, deviceIDBytes, err = loadOrCreateAdoptionUUID(deviceIDFilePath)
	if err != nil {
		log.Fatalf("failed to load/create device-id: %v", err)
	}
	log.Printf("device-id: %s", deviceIDStr)
	adoptionUUID = deviceIDBytes[:]
	loadAdoptionUUID(adoptionUUIDFilePath)

	adoptedStateFilePath = *adoptedStateFile
	loadAdoptedState(adoptedStateFilePath)
	log.Printf("adopted state loaded: %v", isAdopted.Load())

	// Device name defaults to the model string until Protect pushes a real name
	// via ChangeDeviceSettings, which is then persisted.
	loadDeviceName(deviceNameFilePath, cfg.Model)
	log.Printf("device name: %s", getDeviceName())

	if !*noDiscovery {
		go runDiscoveryResponder(macBytes(cfg.MAC), net.ParseIP(cfg.IP), cfg.Model, cfg.FWVersion, cfg.SysID, *guid)
	}

	// Blink while not adopted, steady once adopted.
	go runStatusLedBlinker()

	// Always started, independent of awaitingManage below -- see
	// runManageServer's doc comment.
	manageAwaitFilePath = *manageAwaitFile
	go runManageServer()

	// The firmware's own settings page (webui.go).
	if cfg.WebUIPort > 0 {
		go runWebUI(fmt.Sprintf(":%d", cfg.WebUIPort))
	}

	// The updates companion connection: real ubnt_reportd negotiates
	// subprotocol "logs1" (found via a live heap capture). Keep it.
	go runUpdatesConnection(context.Background())

	// Real hardware waits for the controller's HTTPS adopt push before dialing
	// :7442; off-subnet controllers can't get discovery, so dial tokenless.
	if !isAdopted.Load() {
		if controllerOnLink(cfg.Host) {
			awaitingManage.Store(true)
			log.Printf("camera not yet adopted: answering discovery and waiting for a controller adopt push on :443 (matches real hardware)")
		} else {
			// Broadcast can't reach it and multicast routing isn't guaranteed, so
			// dial :7442 tokenless; the discovery responder still runs.
			log.Printf("controller %s is off-subnet: dialing :7442 tokenless pre-adoption so it can see/adopt this camera", cfg.Host)
		}
	} else if _, err := os.Stat(manageAwaitFilePath); err == nil {
		awaitingManage.Store(true)
		log.Printf("starting in awaiting-manage state (found %s from a prior ResetToDefaults)", manageAwaitFilePath)
	}

	backoff := time.Second
	for {
		if awaitingManage.Load() {
			// Poll rather than block forever, so a manage push delivered at
			// any time is picked up.
			select {
			case res := <-manageCh:
				log.Printf("awaiting controller adopt push on :443 -- received push")
				applyManagePush(res)
				backoff = time.Second
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// Pick up a fresh push even while already adopted and about to dial
		// -- real hardware accepts a re-push at any time.
		select {
		case res := <-manageCh:
			applyManagePush(res)
		default:
		}

		err := run(context.Background())
		if err != nil {
			log.Printf("error, reconnecting in %s: %v", backoff, err)
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// applyManagePush updates cfg from a controller-initiated adopt push; an empty
// Host leaves cfg.Host as-is (guards against a malformed push).
func applyManagePush(res manageResult) {
	cfgMu.Lock()
	cfg.Token = res.Token
	if res.Host != "" {
		cfg.Host = res.Host
		// Persist as the runtime adopt-push pin so this controller "sticks"
		// across restarts. A human-set unifi.cfg CONTROLLER still outranks it.
		if err := os.WriteFile(defaultInformHostFilePath, []byte(res.Host+"\n"), 0600); err != nil {
			log.Printf("applyManagePush: failed to persist inform-host: %v", err)
		}
	}
	cfgMu.Unlock()
	if res.ConsoleID != "" {
		if raw, err := parseUUID(res.ConsoleID); err != nil {
			log.Printf("applyManagePush: mgmt.consoleId %q is not a valid UUID, ignoring: %v", res.ConsoleID, err)
		} else {
			// Only adoptionUUID (0x26 TLV) may track consoleId; deviceIDStr is
			// this camera's WSS device-id and must NOT become the controller's.
			adoptionUUID = raw[:]
			saveAdoptionUUID(adoptionUUIDFilePath, res.ConsoleID)
			log.Printf("applyManagePush: adopted controller's consoleId %s as 0x26 TLV (device-id header stays %s)", res.ConsoleID, deviceIDStr)
		}
	}
	log.Printf("applied manage push: host=%s token=%q", cfg.Host, cfg.Token)
}
