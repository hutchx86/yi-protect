// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// unifi-avclient: UniFi Protect "avclient" adoption/control client for
// Yi-Hack-Allwinner-v2. Protocol/message shapes are ported from unifi-cam-proxy
// (github.com/keshavdv/unifi-cam-proxy, unifi/cams/base.py) and validated
// against this controller. Go (gorilla/websocket), cross-compiled for the
// camera's armv7 target.
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

// unifiPrefix is the project-owned root on the SD card. Everything this
// client persists or shells out to lives under here, not inside a yi-hack
// install.
const unifiPrefix = "/tmp/sd/unifi"

// Codec ALSA capture-gain control driven by setMicLevel (via bin/mixer_set).
// On sun8iw19 (sun8iw19codec, hw:0) the mic/ADC gain is "MIC1 gain volume",
// range 0..31 with the stock 0 dB point at 30 -- the functional analog of the
// UBNT_CVOLUME element a real camera writes. Setting it scales the mic before
// the encoder, so both the AAC and Opus tracks follow the controller volume.
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
	// SysID is the real UBNT catalog hex system id for cfg.Model, e.g.
	// 0xa590 for "UVC G3 Instant", taken from the controller's own model
	// catalog (service.js) and cross-validated against a real G3 Flex's
	// board.sysid. Sent as the `camera-model` WSS handshake header and as
	// discovery's 0x10 TLV -- real cameras send the hex id there, not the
	// model name (that only goes in the JSON hello's "model" field).
	SysID uint16

	// HasPTZ gates the "ptz" featureFlags key -> real hardware capability.
	// Default false; set via -ptz.
	HasPTZ bool

	// IsMediad gates the advanced picture-control path: when true AND the
	// sister project's custom rmm replacement (mediad) is detected installed
	// and running, Protect's picture settings are forwarded to mediad's control
	// socket instead of being acknowledged and ignored. Set via unifi.cfg's
	// IS_MEDIAD=yes or -mediad; see mediad_ctl.go.
	IsMediad bool
}

// cfg.MAC/cfg.IP are last-resort fallbacks, used only when
// detectNetworkIdentity() (called from main()) can't read wlan0/eth0 -- e.g.
// running off-camera for local testing. On real hardware they're always
// overwritten with the detected values.
//
// These are deliberately obviously-fake placeholders: a unit stuck on them is
// instantly recognizable as broken, and no real device's identity can collide
// with them. Do NOT put a real camera's identity here.
var cfg = Config{
	Host:  "10.0.0.1",
	Port:  7442,
	Token: "", // set via -token flag
	MAC:   "DEADDEADBEEF",
	IP:    "0.0.0.0",
	Model: "UVC G3 Instant",
	// "UVC G3 Instant" (sysid 0xa590) maps to platform SAV532Q. Protect
	// parses our reported version and compares it to the newest SAV532Q
	// release in its `updates` table; a version newer than any known build
	// makes Protect offer a "Click to Update" that downgrades us. The
	// suffix below is not the real SAV532Q release, but Protect's comparison
	// appears to look only at major.minor.patch.
	FWVersion: "UVC.SAV532Q.v4.75.62.67.9cdac69.260331.1630",
	CertFile:  unifiPrefix + "/etc/unifi_client_go.crt",
	KeyFile:   unifiPrefix + "/etc/unifi_client_go.key",
	SysID:     0xa590,
}

// cfgMu guards cfg against the one real concurrent access:
// applyManagePush mutates cfg.Host/cfg.Token from main()'s goroutine while
// runUpdatesConnection reads cfg from its own goroutine. run() is still only
// called from main()'s goroutine, so its reads remain unguarded.
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

// adoptionUUIDFilePath persists the controller's consoleId -- real hardware
// advertises it as discovery TLV 0x26 once adopted. Persisted so a reboot
// while adopted doesn't make the first discovery replies send the device-id
// and appear "adopted to another console".
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
	// sendMu guards msgID and every ws.WriteJSON call. startMotionWatcher
	// pushes unsolicited EventAnalytics messages from its own goroutine
	// concurrently with process()'s read loop, and gorilla/websocket requires
	// all writes to be externally synchronized.
	sendMu    sync.Mutex
	streamsMu sync.Mutex
	streams   map[string]string // stream key (video1/2/3) -> assigned streamName, once a real destination is set

	// video1/video2's currently-active FlvPush destination/token, so repeated
	// ChangeVideoSettings for an UNCHANGED assignment (the controller resends
	// them while a tier is actively viewed) don't force a needless FlvPush
	// reconnect -- an unconditional reconnect-per-message killed a healthy
	// stream every ~10-15s during viewing. Guards the activeVideoN* fields,
	// read from process() and the video handler.
	videoMu                sync.Mutex
	activeVideo1Host       string
	activeVideo1StreamName string
	activeVideo2Host       string
	activeVideo2StreamName string
	// "video3" ("medium" on the FlvPush wire side) is requested by Protect's
	// expanded/single-camera panel regardless of the quality dropdown, while
	// video1/video2 only serve the grid view. Aliased to the same real LOW
	// (640x360) frames as video2, matching video3's declared 640x360 schema --
	// a second destination for the same frames, not a distinct resolution.
	activeVideo3Host       string
	activeVideo3StreamName string

	// mTLS-enabled HTTP client (same cert as the WSS connection) for
	// snapshot uploads -- see handleGetRequest().
	httpClient *http.Client

	// snapshotGrabMu serializes imggrabber runs. Concurrent GetRequests would
	// each fork a ~9MB imggrabber and race for the shared sensor/ISP; on this
	// 60MB board that OOM-kills rmm.
	snapshotGrabMu sync.Mutex
	// snapshotMu guards the last-good JPEG cache below -- a slightly-stale
	// frame is better than no thumbnail when a grab misses its keyframe.
	snapshotMu   sync.Mutex
	snapshotJPEG map[string][]byte
	snapshotAt   map[string]time.Time

	// Microphone state, driven by ChangeVideoSettings' `audio` block. The
	// controller sends {bitRate, volume}; micVolume remembers the raw value so
	// a later message with no audio block echoes the real state instead of
	// resetting to 100. micLevel is the effective level after the Protect
	// 1..100 -> mute/gain remap (micLevelFromVolume); micLevelSet suppresses
	// repeat MUTE/gain writes.
	micMu       sync.Mutex
	micVolume   int
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
// derivation; it only needs to stay constant so the same serial derives the
// same device-id.
var deviceIDNamespace = [16]byte{0x8b, 0x1a, 0x9d, 0x53, 0x6c, 0x4a, 0x40, 0x8e, 0xb0, 0x3a, 0xd8, 0x4c, 0xf4, 0x0d, 0x1e, 0x6f}

// newUUIDv5 derives a deterministic RFC-4122 v5 UUID from name, namespaced
// under deviceIDNamespace, so the same physical camera always derives the same
// device-id from its own hardware serial.
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

// readHardwareSerial reads this camera's factory-programmed serial, the same
// way yi-hack's service.sh does: find the "mfg@<partition>" token in the
// kernel bootargs, then read 20 bytes at offset 36 from that raw partition
// device. Genuinely unique per physical unit.
func readHardwareSerial() (string, error) {
	bootargs, err := os.ReadFile("/sys/firmware/devicetree/base/chosen/bootargs")
	if err != nil {
		return "", fmt.Errorf("read bootargs: %w", err)
	}
	m := regexp.MustCompile(`mfg@(\S+)`).FindSubmatch(bootargs)
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

// modelSuffixFilePath records the real physical camera model (e.g.
// y623/h52ga/r35gb), distinct from the spoofed cfg.Model/cfg.SysID. Used to
// pick the correct sensor resolution profile and imggrabber/unifi_flv_bridge
// -m argument.
const modelSuffixFilePath = unifiPrefix + "/etc/model_suffix"

// readHardwareModel returns this camera's physical model (from
// modelSuffixFilePath), falling back to "y623" (the project's reference
// sensor profile) if the file is missing or empty.
func readHardwareModel() string {
	if b, err := os.ReadFile(modelSuffixFilePath); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "y623"
}

// loadOrCreateAdoptionUUID returns a stable per-device UUID (string and raw
// bytes), persisted next to the client cert so it survives restarts and
// reboots. Used as the `device-id` WSS header and, once adopted, discovery's
// 0x26 TLV.
func loadOrCreateAdoptionUUID(path string) (string, [16]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(data))
		if raw, perr := parseUUID(s); perr == nil {
			return s, raw, nil
		}
		log.Printf("device-id file %s unreadable (%v), regenerating", path, err)
	}

	// Prefer deriving from the camera's real hardware serial over a random
	// UUID: unique by construction, reproducible from hardware if the file is
	// ever lost, and traceable to a physical unit. Random UUIDv4 is the
	// fallback when the serial can't be read (e.g. running off-camera).
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

// Identity lifecycle on factory reset (ResetToDefaults): performReset ->
// deleteIdentity removes the cert/key AND the derived device-id, then reboots;
// on next boot ensureCertKey mints a fresh pair and
// loadOrCreateAdoptionUUID re-derives the device-id. A factory-reset device
// comes back with no identity and re-provisions fresh.
//
// generateSelfSignedECDSACert makes a fresh self-signed ECDSA P-256 pair,
// PEM-encoded in memory. It MUST stay ECDSA P-256: an earlier RSA-2048 keygen
// hung for minutes on this device's single slow ARM core, freezing ping/pong
// until the controller reaped the connection. Used by ensureCertKey and the
// manage-API HTTPS listener.
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

// deleteIdentity removes the persisted identity material -- the cert/key pair
// AND the derived device-id -- so a factory-reset device comes back with no
// identity and re-provisions fresh on next boot. Called from performReset.
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

// ensureCertKey generates a fresh self-signed mTLS cert/key pair in place if
// either file is missing (the first boot after a factory reset, or a first
// install). Idempotent -- an existing pair is left untouched.
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

// performReset runs deleteIdentity off the message-read loop's hot path,
// switches the client into "awaiting controller-initiated adopt" mode (see
// manage.go), then reboots -- matching real hardware on ResetToDefaults.
// Identity is deleted before the reboot so no stale identity survives;
// ensureCertKey/loadOrCreateAdoptionUUID re-provision on the next boot.
//
// Regenerating identity alone does NOT stop instant re-adoption: the
// controller's acceptance is keyed on MAC and the client keeps dialing
// regardless. Real hardware stops dialing :7442 on release/reset and waits for
// a controller HTTPS adopt push on :443; entering awaiting-manage makes the
// dial loop actually stop.
//
// Uses /sbin/reboot directly so it works even if the SD-card scripts are
// mid-edit or broken.
func (c *Client) performReset() {
	if err := c.deleteIdentity(); err != nil {
		log.Printf("ResetToDefaults: failed to delete identity: %v", err)
	}

	enterAwaitingManage("ResetToDefaults")

	// If the reboot command below fails, this process keeps running;
	// awaitingManage (set by enterAwaitingManage) is what gates the dial
	// loop in main(). Clearing cfg.Token here just keeps state consistent in
	// that fallback case.
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

// readWifiStatus shells out to iwconfig and parses live association state off
// wlan0. Returns nil if wlan0 isn't associated (e.g. a wired build) rather
// than fabricating values. Matches the controller's NetworkStatus field set
// (linkSpeedMbps/channel/essid/frequency/signalLevel/bssid).
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

// featureFlags' keys are read by the controller's own service.js as
// hasX:Boolean(t.<key>). "truedaynight" is the real IR/night-vision key (not
// "infrared"/"ir"); "ledIR" is the IR illuminator, paired with cpld_ctl's
// led/ircut; "ledStatus" is the separate status LED (ipc_cmd -l).
func featureFlags() map[string]interface{} {
	flags := map[string]interface{}{
		"mic":          true,
		"speaker":      true,
		"truedaynight": true,
		"ledIR":        true,
		"ledStatus":    true, // hasLedStatus <- t.ledStatus; real hardware, ipc_cmd -l
		"wifi":         true, // hasWifi <- t.wifi; real hardware capability (8189fs module)
		// bluetooth/hdr/privacyMask/autoICROnly: real G3 Instant declares all
		// four true; added here to match. None have behavior wired up yet --
		// self-declared metadata only, like wifi/ledStatus above.
		"bluetooth":    true,
		"hdr":          true,
		"privacyMask":  true,
		"autoICROnly":  true,
		"aec":          []string{"wideband"}, // feature_aec_wideband=1 on real hardware; narrowband/fullband both 0
		"videoMode":    []string{"default", "sport", "slowShutter"},
		"motionDetect": []string{"enhanced"},

		// Fields copied from a real G3 Instant's /usr/etc/features.json, which
		// ubnt_avclient merges into its hello `features`. Their absence made
		// Protect show empty codec lists; the empty `audioCodecs` also gated
		// the Automatic-quality talkback button. Real G3 hardware sends only
		// AAC, but advertising opus matches the shipped file and keeps
		// talkback gating happy.
		"audioCodecs":           []string{"aac", "opus"},
		"videoCodecs":           []string{"h264", "mjpg"},
		"opusSampleRates":       []int{16000},
		"aecTalkbackSwitch":     false,
		"videoSourceCount":      1,
		"videoModeMaxFps":       []int{30, 30, 20},
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

// talkbackSettingsDefaults mirrors a real camera's reported values.
// samplingRate is this device's native PCM rate (16000), not the 22050 a real
// camera reported, since /tmp/audio_in_fifo takes raw PCM with no resampling.
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
		// The hello reply carries the controller's own UUID as
		// `controllerUuid`; real hardware advertises it as discovery TLV
		// 0x26, so capture and persist it.
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
		// With mediad, restore the vendor defaults on its control socket too;
		// the response is the static schema either way. Off the read loop so a
		// slow socket can't delay the ack.
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
		// A real name set/changed in the Protect app arrives here; persist it
		// instead of echoing the old value. Falls back to the current name
		// (model string on a fresh device) if this push carries no "name".
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
		osd := map[string]interface{}{
			"enableDate": 1, "enableLogo": 1, "enableReportdStatsLevel": 0,
			"enableStreamerStatsLevel": 0, "tag": getDeviceName(),
		}
		return false, c.send(c.genResponse("ChangeOsdSettings", m.MessageID, map[string]interface{}{
			"_1": osd, "_2": osd, "_3": osd, "_4": osd,
			"enableOverlay": 1, "logoScale": 50, "overlayColorId": 0,
			"textScale": 50, "useCustomLogo": 0,
		}))
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
		// PanTiltReset has no ipc_cmd equivalent beyond what
		// GetCurrentPosition/Preset cover, and Enable/DisablePtzControl look
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
		return false, c.send(c.genResponse("UpdateUsernamePassword", m.MessageID, nil))
	case "ChangeTalkbackSettings":
		if raw, err := json.Marshal(m.Payload); err == nil {
			log.Printf("ChangeTalkbackSettings raw payload: %s", raw)
		}
		if m.ResponseExpected {
			// Accept the controller's requested settings instead of
			// overriding them with our defaults. The controller asks for
			// opus/serverudp-rtp/24000 while talkbackSettingsDefaults answers
			// aac/serverudp/16000; talkback_rx handles both transports, but
			// answering with a different transport is a real negotiation
			// mismatch and the leading suspect for talkback never activating.
			resp := talkbackSettingsDefaults()
			for k, v := range m.Payload {
				resp[k] = v
			}
			return false, c.send(c.genResponse(fn, m.MessageID, resp))
		}
		return false, nil
	case "ChangeBrightnessSettings":
		// Legacy separate brightness message. Schema not captured; log the raw
		// payload and run it through the same picture-control mapping so it
		// reaches mediad when the advanced path is enabled. It is a PARTIAL
		// object, so it must not end the delta seed (see mediadApplyControls).
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
		// Not a real upgrade: fetch the version string out of the update image
		// the controller points at and adopt it (matching unifi-cam-proxy's
		// process_upgrade). Real update images for real models aren't
		// encrypted, but every model+version pairing we have points at an
		// encrypted image, so looksLikeVersionString never succeeds and the
		// controller re-issues this on every reconnect. unifi-cam-proxy
		// force-reconnects here; since we can never satisfy the check that
		// would mean disconnecting forever, stay connected and ignore instead.
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
		// Must not block: process() runs inline in run()'s ReadMessage loop,
		// and gorilla only handles ping/pong inside ReadMessage -- a hung
		// handleGetRequest would freeze the whole connection and trip the
		// controller's missed-pong timeout, not just delay the snapshot.
		go c.handleGetRequest(m)
		if m.ResponseExpected {
			return false, c.send(c.genResponse(fn, m.MessageID, nil))
		}
		return false, nil
	case "ResetToDefaults":
		// Sent by the controller when an admin removes this camera from
		// Protect. Real hardware stops dialing :7442 on this message and waits
		// for a controller HTTPS adopt push on :443; performReset puts us in
		// that same awaiting-manage state (manage.go) and deletes identity, so
		// the dial loop in main() actually stops instead of instantly
		// reconnecting.
		//
		// Must run off this goroutine: performReset reboots the device and
		// flips state, so it runs in the background. This case returns
		// immediately (false, not forceReconnect) so ReadMessage keeps
		// servicing pings; performReset closes the connection itself, which
		// triggers the real reconnect.
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

// handleVideoSettings responds to ChangeVideoSettings.
//
// The CLIENT is the authority on its own video capabilities: always send back
// a complete, hardcoded video1/video2/video3/mjpg schema regardless of the
// incoming request. The payload (when non-empty) only supplies destinations to
// merge in. Echoing an empty request back as literal null video/audio made the
// controller truncate adoption and close within ~1s.
//
// runCpldCtl shells out to cpld_ctl (work/cpld_ctl/cpld_ctl.c), which issues
// the confirmed /dev/cpld_periph ioctls for the IR-cut filter and IR LED
// array. Logs failures rather than returning an error -- callers fire these
// best-effort alongside a ChangeIspSettings ack that must go out regardless.
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

// handleIspSettings wires ChangeIspSettings' irLedMode/irLedLevel fields to
// real hardware via cpld_ctl (work/cpld_ctl/cpld_ctl.c), which issues
// ioctl()s against /dev/cpld_periph: 0x7015/0x7016 for the mechanical IR-cut
// filter and 0x7013 for the IR LED array. Real UniFi cameras couple filter and
// LED under one manual-night-vision control (the Always On/Off extremes), so
// manual mode drives both together.
//
// "auto" covers plain Auto (icrSwitchMode=="sensitivity", left to rmm's native
// day/night switching) and Custom with a real 1-30 lux threshold
// (icrSwitchMode=="lux", icrCustomValue=slider) -- see lux.go.
func (c *Client) handleIspSettings(m Envelope) error {
	if raw, err := json.Marshal(m.Payload); err == nil {
		log.Printf("ChangeIspSettings raw payload: %s", raw)
	}

	// Night vision: when mediad is the producer it owns the applier (CPLD
	// filter/LED + day/night ISP tuning) via the nightvision/night_lux/ir_led
	// controls, so skip the local cpld_ctl/lux path and make sure the local lux
	// poller is off -- otherwise the two would fight. With stock rmm, keep the
	// historical local behavior (cpld_ctl + lux.go) unchanged.
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

	// Forward the picture + night-vision controls to mediad when the advanced
	// path is live. Off the read loop: the dial is local, but a stalled mediad
	// must never delay the controller's ack. See mediad_ctl.go for the mapping.
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
// controller's ledFaceEnabled (0/1) field.
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

	ledVal := 0
	if ledOn {
		ledVal = 1
	}
	return map[string]interface{}{
		"ledFaceAlwaysOnWhenManaged": 1, "ledFaceEnabled": ledVal, "speakerEnabled": 1,
		"speakerVolume": 100, "systemSoundsEnabled": 1, "userLedBlinkPeriodMs": 0,
		"userLedColorFg": "blue", "userLedOnNoff": ledVal,
	}
}

func (c *Client) handleVideoSettings(m Envelope) error {
	if raw, err := json.Marshal(m.Payload); err == nil {
		log.Printf("ChangeVideoSettings raw payload: %s", raw)
	}

	// Microphone: the controller pushes {audio:{bitRate,volume}} when the mic
	// is muted/unmuted (volume==0 means disabled). Real hardware applies that
	// as ADC capture gain 0, so the audio track keeps flowing but silent;
	// FlvPush reproduces that (dropping tags would trip evostreamms's 1000 ms
	// video-vs-last-audio threshold). An absent audio block leaves the last
	// known value intact so the response below doesn't lie about it.
	audioVolume := 100
	c.micMu.Lock()
	audioVolume = c.micVolume
	hasAudio := false
	if audio, ok := m.Payload["audio"].(map[string]interface{}); ok {
		hasAudio = true
		if v, ok := audio["volume"].(float64); ok {
			audioVolume = int(v)
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

	// Real HIGH-channel (video1) resolution differs per physical model: y623
	// (GC3003) is 2304x1296, h52ga (GC2053) is 1920x1080. Confirmed by
	// decoding a real SPS NAL from each camera's encoder output. readHardwareModel
	// is the real hardware model, distinct from the spoofed cfg.Model/cfg.SysID.
	hwModel := readHardwareModel()
	video1Width, video1Height := 2304, 1296
	if hwModel == "h52ga" {
		video1Width, video1Height = 1920, 1080
	}

	if video, ok := m.Payload["video"].(map[string]interface{}); ok {
		// Shutter exposure (Video Mode) and the HIGH bitrate -> mediad, in one
		// delta-gated batch so the controller's periodic resend of the whole
		// video object doesn't re-issue them. No-op unless the advanced path is
		// live.
		var vidControls []mediadCtl
		vidControls = append(vidControls, shutterControls(video)...)
		if v1, ok := video["video1"].(map[string]interface{}); ok {
			if bps := videoStreamBitrate(v1); bps > 0 {
				vidControls = append(vidControls, mediadCtl{key: "bitrate", value: clampBitrate(bps)})
			}
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
			// video1 (HIGH) and video2 (LOW, real 640x360) feed real FlvPush
			// sources. video3 ("medium" on the wire) is aliased to the same
			// real LOW frames as video2: Protect's expanded/single-camera
			// panel requests video3 for every quality option, while
			// video1/video2 only serve the grid view. Not a distinct
			// resolution, just a second destination for the same output.
			//
			// streamName is the controller-issued per-session token
			// (avSerializer.parameters.streamName), used as the FLV onMetaData
			// streamName. Only reconnect FlvPush when the destination/token
			// actually changed.
			c.videoMu.Lock()
			if key == "video1" {
				if u.Host != c.activeVideo1Host || streamName != c.activeVideo1StreamName {
					c.activeVideo1Host = u.Host
					c.activeVideo1StreamName = streamName
					c.videoMu.Unlock()
					startVideoStream(u.Host, streamName, "high")
				} else {
					c.videoMu.Unlock()
				}
			} else if key == "video2" {
				if u.Host != c.activeVideo2Host || streamName != c.activeVideo2StreamName {
					c.activeVideo2Host = u.Host
					c.activeVideo2StreamName = streamName
					c.videoMu.Unlock()
					startVideoStream(u.Host, streamName, "low")
				} else {
					c.videoMu.Unlock()
				}
			} else if key == "video3" {
				if u.Host != c.activeVideo3Host || streamName != c.activeVideo3StreamName {
					c.activeVideo3Host = u.Host
					c.activeVideo3StreamName = streamName
					c.videoMu.Unlock()
					startVideoStream(u.Host, streamName, "medium")
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
		"audio": map[string]interface{}{
			"bitRate": 32000, "channels": 1, "description": "audio track",
			"enableTemporalNoiseShaping": false, "enabled": true, "mode": 0,
			"quality": 0, "sampleRate": 16000, "type": "aac", "volume": audioVolume,
		},
		"video": map[string]interface{}{
			"enableHrd": false, "hdrMode": 0, "lowDelay": false,
			"videoMode": "default", "vinFps": 30,
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
				"M": 1, "N": 30,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video1"]},
					"parameters":   streamParams("video1"),
					"type":         "extendedFlv",
				},
				"bitRateCbrAvg": 1400000, "bitRateVbrMax": 2800000, "bitRateVbrMin": 48000,
				"description": "Hi quality video track", "enabled": true, "fps": 15,
				"gopModel": 0, "height": video1Height, "horizontalFlip": false,
				"isCbr": false, "maxFps": 30, "minClientAdaptiveBitRate": 0,
				"minMotionAdaptiveBitRate": 0, "nMultiplier": 6, "name": "video1",
				"sourceId": 0, "streamId": 1, "streamOrdinal": 0, "type": "h264",
				"validBitrateRangeMax": 2800000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{1, 2, 3, 4, 5, 6, 8, 9, 10, 12, 15, 16, 18, 20, 24, 25, 30},
				"verticalFlip":   false,
				"width":          video1Width,
			},
			"video2": map[string]interface{}{
				"M": 1, "N": 30,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video2"]},
					"parameters":   streamParams("video2"),
					"type":         "extendedFlv",
				},
				// Real LOW resolution (640x360) -- must match the encoder.
				"bitRateCbrAvg": 500000, "bitRateVbrMax": 750000, "bitRateVbrMin": 48000,
				"currentVbrBitrate": 500000, "description": "Low quality video track",
				"enabled": true, "fps": 15, "gopModel": 0, "height": 360,
				"horizontalFlip": false, "isCbr": false, "maxFps": 30,
				"minClientAdaptiveBitRate": 0, "minMotionAdaptiveBitRate": 0, "nMultiplier": 6,
				"name": "video2", "sourceId": 1, "streamId": 2, "streamOrdinal": 1, "type": "h264",
				"validBitrateRangeMax": 750000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{1, 2, 3, 4, 5, 6, 8, 9, 10, 12, 15, 16, 18, 20, 24, 25, 30},
				"verticalFlip":   false,
				"width":          640,
			},
			"video3": map[string]interface{}{
				"M": 1, "N": 30,
				"avSerializer": map[string]interface{}{
					"destinations": []string{vidDst["video3"]},
					"parameters":   streamParams("video3"),
					"type":         "extendedFlv",
				},
				"bitRateCbrAvg": 300000, "bitRateVbrMax": 200000, "bitRateVbrMin": 48000,
				"currentVbrBitrate": 200000, "description": "Low quality video track",
				"enabled": true, "fps": 15, "gopModel": 0, "height": 360,
				"horizontalFlip": false, "isCbr": false, "maxFps": 30,
				"minClientAdaptiveBitRate": 0, "minMotionAdaptiveBitRate": 0, "nMultiplier": 6,
				"name": "video3", "sourceId": 2, "streamId": 4, "streamOrdinal": 2, "type": "h264",
				"validBitrateRangeMax": 750000, "validBitrateRangeMin": 32000,
				"validFpsValues": []int{1, 2, 3, 4, 5, 6, 8, 9, 10, 12, 15, 16, 18, 20, 24, 25, 30},
				"verticalFlip":   false,
				"width":          640,
			},
		},
	}
	return c.send(c.genResponse("ChangeVideoSettings", m.MessageID, payload))
}

// handleGetRequest answers "GetRequest" (observed live payload:
// {"quality":"medium","timeoutMs":60000,"uri":"https://.../internal/
// camera-upload/<token>","what":"snapshot"}). The content isn't returned over
// the WS at all: the client grabs a JPEG and HTTP POSTs it (multipart/
// form-data, field "payload") to the uri using the SAME mTLS client cert as
// the WSS connection (matching unifi-cam-proxy). The WS response, sent by the
// caller in process(), is a bare ack.
//
// The JPEG comes from the stock imggrabber binary, which blocks until the next
// SPS/IDR lands, so its latency tracks the encoder's IDR interval: `-r low`
// ~0.2s, `-r high` 2-9s (the HIGH GOP is ~9s). The controller grants
// timeoutMs:60000; honor it (clamped) and keep the last good JPEG per
// resolution as a fallback so a reconfigure-storm miss still yields a thumbnail.
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

// snapshotTimeout returns the deadline to give imggrabber, honoring the
// controller's own timeoutMs (60000 live) but clamping to a sane range. Waiting
// for the next IDR is normal, not a hang, so this is deliberately generous.
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

// looksLikeVersionString is a sanity check on bytes pulled from a firmware
// image at a fixed offset -- real images may be encrypted and have no plain
// version there.
func looksLikeVersionString(v string) bool {
	if len(v) < 3 || len(v) > 64 {
		return false
	}
	// Real version strings (e.g. "UVC.S2L.v4.75.66.67.9cdac69.260331.1630")
	// start with a letter and contain dots; garbage pulled from an encrypted
	// image (e.g. a sha256-looking hex digest) won't match both.
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

// fetchFirmwareVersion reads the version string embedded near the start of a
// real firmware image, matching unifi-cam-proxy's process_upgrade(): bytes
// 4-53 (after a Range request for the first 100 bytes) hold the null-padded
// version string.
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

// FlvPush control FIFO: our native C++ module inside the deployed
// unifi_flv_bridge binary (work/flv_bridge/ -- FshareReader.cpp reads the
// stock encoder's shared-memory frame ring, FlvPush.cpp muxes frames into FLV
// tags written to a TCP socket it dials itself; no ffmpeg, no RTSP round-trip).
// An earlier ffmpeg-spawning relay exhausted this 60MB device's RAM. Writing
// one line to the FIFO costs nothing.
const flvPushFifo = "/tmp/unifi_flv_bridge_ctl"

var (
	videoStreamMu sync.Mutex
	// A FIFO reader (FlvPush.cpp's ctlThreadMain) only has the pipe open for a
	// brief window between one writer's EOF and its next fopen(), so opening
	// per command raced that window and failed with ENXIO under the
	// controller's normal reassignment cadence. Holding one writer open for
	// the client's lifetime sidesteps the race.
	flvPushFifoFile *os.File
	// currentMicLevel mirrors the effective mic level applied to FlvPush and
	// the codec gain so startVideoStream can re-assert both on a fresh CONNECT
	// (guarded by videoStreamMu). Defaults to full so a CONNECT before the
	// controller's first audio settings doesn't silence the mic.
	currentMicLevel = 100
)

// channel is "high", "low", or "medium" -- FlvPush tracks each destination
// separately, so they can be connected/reconnected independently.
func startVideoStream(dest, streamName, channel string) {
	videoStreamMu.Lock()
	defer videoStreamMu.Unlock()
	cmd := fmt.Sprintf("CONNECT %s %s %s\n", dest, streamName, channel)
	if err := writeFlvPushFifoLocked(cmd); err != nil {
		log.Printf("startVideoStream[%s]: failed to write FlvPush FIFO: %v", channel, err)
		return
	}
	// Re-assert the mic state on every CONNECT: FlvPush's flag is
	// process-global and resets if the bridge restarts (while setMicLevel only
	// writes on change), and the codec gain is re-applied in case the encoder
	// re-initialised the mixer.
	arg := "off"
	if currentMicLevel <= 0 {
		arg = "on"
	}
	if err := writeFlvPushFifoLocked(fmt.Sprintf("MUTE %s\n", arg)); err != nil {
		log.Printf("startVideoStream[%s]: failed to re-assert mic mute: %v", channel, err)
	}
	applyMicGain(currentMicLevel)
	log.Printf("startVideoStream[%s]: told FlvPush to push to %s, streamName=%s", channel, dest, streamName)
}

func stopVideoStream(channel string) {
	videoStreamMu.Lock()
	defer videoStreamMu.Unlock()
	if err := writeFlvPushFifoLocked(fmt.Sprintf("DISCONNECT %s\n", channel)); err != nil {
		log.Printf("stopVideoStream[%s]: failed to write FlvPush FIFO: %v", channel, err)
	}
}

// micLevelFromVolume maps the controller's microphone volume onto our
// effective level (0..100, where 0 means mute). Protect's Microphone Level
// slider is 1..100 and never offers 0, so its minimum is treated as mute --
// otherwise the mic could not be silenced from the UI. The rest maps linearly.
func micLevelFromVolume(volume int) int {
	if volume <= 1 {
		return 0
	}
	if volume > 100 {
		return 100
	}
	return volume
}

// setMicLevel applies an effective mic level (0..100). FlvPush substitutes
// silent audio at level 0 (its `MUTE on|off` FIFO command), and the codec
// capture gain is set proportionally via bin/mixer_set, mirroring a real
// camera's linear mapping of audio.volume onto the hardware capture element
// (upstream of the encoder, so both the AAC and Opus tracks follow). No-op
// unless the level changes: ChangeVideoSettings resends the same audio block.
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

// applyMicGain writes the level as a percentage of the codec capture-gain
// element via bin/mixer_set (see micGainCard/micGainControl). Best-effort: a
// failure is logged, not fatal. Caller must hold videoStreamMu.
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
	// "adopted" must NOT be `cfg.Token == ""` (dialing with no token): a
	// manage-API push populates cfg.Token and it never goes back to empty.
	// Use the persisted isAdopted flag, set true exactly when a real
	// ubnt_avclient_paramAgreement lands.
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
		micVolume: 100,
	}

	// Seed the mediad control delta on every fresh WSS connection: the
	// controller's first ChangeIspSettings/ChangeVideoSettings after connect is
	// the full object, and must be recorded rather than re-applied (it would
	// otherwise burst mediad with every non-default field at once).
	resetMediadDelta()

	// Real hardware asks the controller to correct its clock before it
	// introduces itself (see timesync.go). Without this the camera's clock
	// stays at its firmware build date and the controller rejects every
	// pushed frame with a multi-year wc/now diff.
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

// runUpdatesConnection is real hardware's second, independent WSS connection
// -- owned there by /bin/ubnt_reportd, not ubnt_avclient. Same host/port/path
// as the main connection (/camera/1.0/ws), but with no ?token=, a bare
// `Camera-MAC` header, and subprotocol "logs1" instead of "secure_transfer".
// `ds` labels a connection matching this shape "-updates" and treats it as a
// companion rather than a session replacement for the main one.
//
// Runs for the process lifetime, independent of adoption state, and sends
// nothing after connecting; real ubnt_reportd carries no application payload
// either, just lws-level keepalive, which gorilla's ping/pong satisfies.
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
	// A client cert is REQUIRED here -- without one, TLS itself fails
	// ("remote error: tls: certificate required") before any WS/header logic
	// runs. Real hardware shares one cert between ubnt_avclient and
	// ubnt_reportd, so the cert is not what lets ds treat the two connections
	// as independent.
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
		// ubnt_reportd negotiates subprotocol "logs1" (found via a live heap
		// read), not the main connection's "secure_transfer" -- the leading
		// candidate for the signal ds uses to treat this as an independent
		// companion rather than a session replacement.
		Subprotocols: []string{"logs1"},
	}

	u := fmt.Sprintf("wss://%s:%d/camera/1.0/ws", c.Host, c.Port)
	headers := http.Header{}
	headers.Set("Camera-MAC", c.MAC)
	// Real ubnt_reportd's Host header is bare (e.g. "Host: 10.0.0.1", no
	// port); override gorilla's host:port default in case the backend's
	// connection classification keys off it.
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

	// This connection carries no application-level payload (matching real
	// ubnt_reportd), so without keepalive it idles out and the controller
	// stops treating it as a live companion. Real hardware's lws layer pings.
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

// detectInterfaceIdentity reads one named interface's real MAC and IPv4
// address. Split out of detectNetworkIdentity() so that function can try
// multiple interfaces in preference order.
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

// detectNetworkIdentity reads the camera's real MAC and IPv4. Ethernet is
// preferred: a unit with a plugged-in eth0 (usable IPv4) presents that; wlan0
// is the fallback for WiFi-only setups. Placeholder cfg.MAC/cfg.IP is the last
// resort if neither works.
func detectNetworkIdentity() (mac string, ip string, err error) {
	if mac, ip, err := detectInterfaceIdentity("eth0"); err == nil {
		return mac, ip, nil
	}
	if mac, ip, err := detectInterfaceIdentity("wlan0"); err == nil {
		return mac, ip, nil
	}
	return "", "", fmt.Errorf("neither wlan0 nor eth0 has a usable IPv4 address")
}

// controllerOnLink reports whether host shares a subnet with one of this
// camera's interfaces. Discovery UDP :10001 is broadcast and can't cross
// subnets, so an off-subnet controller can't discover a waiting camera -- it
// must dial out tokenless instead. A hostname or unparseable host is treated
// as on-link, preserving the gating default.
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

// cameraConfigFilePath is the LEGACY, per-file home of the camera-identity
// overrides (MODEL, SYSID hex, FWVERSION), superseded by unifi.cfg. Still read
// as a migration fallback for a deployment that has one. Missing is not an
// error.
const cameraConfigFilePath = unifiPrefix + "/etc/unifi_client_go.camera-config"

// unifiConfigFilePath is the project's single human-edited config file: plain
// KEY=value, '#' comments, blank lines ignored. The shell scripts
// (init.sh/watchdog.sh/wifidhcp.sh/ethdhcp.sh) and this client both read it.
// Keys this client recognizes: MODEL, SYSID, FWVERSION, CONTROLLER.
// Runtime-generated state lives in its own files.
const unifiConfigFilePath = unifiPrefix + "/etc/unifi.cfg"

// readUnifiCfg parses unifi.cfg's flat KEY=value lines into a map (keys
// upper-cased). Missing file or unparsable lines are non-fatal; unknown keys
// are ignored. Later duplicate keys win.
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

// isTruthy parses a human-edited config boolean. Accepts the yes/no form used
// by the shell scripts (and this file's own PTZ key) plus the usual
// true/false/1/0/on/off aliases; anything else is false.
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

// defaultInformHostFilePath is runtime state, not admin config: written by
// applyManagePush when the controller pushes an adopt that pins this camera, so
// the pin survives restarts. A human-set pin belongs in unifi.cfg's CONTROLLER
// key, which outranks this file.
const defaultInformHostFilePath = unifiPrefix + "/etc/unifi_client_go.inform-host"

// dhcpInformHostFilePath is the lowest-priority controller source, written
// automatically by default.script's DHCP-option-43 handling. Kept apart from
// defaultInformHostFilePath so a lease renewal can never clobber the adopt-push
// pin. Consulted only when neither unifi.cfg CONTROLLER nor
// defaultInformHostFilePath is present.
const dhcpInformHostFilePath = unifiPrefix + "/etc/unifi_client_go.inform-host-dhcp"

// loadInformHostOverride reads a plain-text "host" or "host:port" line from
// path (blank/missing: ok=false, caller keeps the compiled-in default).
// Comment lines (leading '#') and blank lines are skipped, so the file can
// carry a short header. A missing file is tolerated, but other errors are
// logged as likely mistakes.
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
	// Wait for an interface to actually have an address before latching the
	// identity. At boot the app can start before DHCP completes; a one-shot
	// check would fall through to the placeholder default for the whole
	// process lifetime, so the controller saw a bogus device.
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

	// Read project config BEFORE computing flag defaults so an explicit
	// -host/-model still wins. Identity precedence: unifi.cfg > legacy
	// camera-config > compiled-in default. Controller precedence, most
	// specific wins: unifi.cfg CONTROLLER (manual pin) > inform-host
	// (runtime adopt-push pin) > inform-host-dhcp (DHCP option 43). The two
	// inform-host files stay separate because they're runtime state, not
	// human config.
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
	cfg.HasPTZ = *ptz
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

	// Device name defaults to the model string until the controller (Protect)
	// pushes a real name via ChangeDeviceSettings, which is persisted from
	// then on.
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

	// The updates companion connection: real ubnt_reportd negotiates
	// subprotocol "logs1" (found via a live heap capture). Keep it.
	go runUpdatesConnection(context.Background())

	// Real hardware does not open the main :7442 control connection until the
	// controller has provisioned it. A factory-fresh (or removed/reset) camera
	// answers UDP :10001 discovery and waits for the controller's HTTPS adopt
	// push on :443, then dials. Dialing tokenless pre-adoption instead makes
	// Protect build the pending device row from that inbound WSS attempt --
	// which ds rejects 403 until an admin clicks Adopt -- and the row ends up
	// with an empty type/name and host 127.0.0.1 (the internal ds->ms hop)
	// instead of the real model/IP Protect learned from discovery. A
	// discovery-sourced row is what real hardware gets, and why the first
	// Adopt click succeeds.
	//
	// EXCEPTION: an off-subnet controller's discovery broadcast can't reach
	// this camera, so waiting would leave it permanently invisible -- dial
	// tokenless pre-adoption so the remote controller learns the device exists.
	if !isAdopted.Load() {
		if controllerOnLink(cfg.Host) {
			awaitingManage.Store(true)
			log.Printf("camera not yet adopted: answering discovery and waiting for a controller adopt push on :443 (matches real hardware)")
		} else {
			// Off-subnet: limited broadcast can't reach the controller and
			// multicast routing isn't guaranteed, so dial :7442 tokenless
			// pre-adoption -- that inbound attempt is how the remote
			// controller learns the device exists. The discovery responder
			// still runs in case multicast is routed.
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

// applyManagePush updates cfg from a controller-initiated adopt push
// (manage.go). An empty Host leaves cfg.Host as-is -- both manage API shapes
// always send a host, but this guards against a malformed push pointing the
// client at an empty address.
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
			// Only adoptionUUID (the 0x26 TLV's raw bytes) may track
			// consoleId. deviceIDStr must NOT be overwritten with it: that is
			// the WSS `device-id` header identifying this camera. Since
			// consoleId is the CONTROLLER's identity, overwriting it made
			// every camera adopted by the same controller send an identical
			// device-id, and the controller closed them with "1008 policy
			// violation: superseded by newer connection for same device".
			adoptionUUID = raw[:]
			saveAdoptionUUID(adoptionUUIDFilePath, res.ConsoleID)
			log.Printf("applyManagePush: adopted controller's consoleId %s as 0x26 TLV (device-id header stays %s)", res.ConsoleID, deviceIDStr)
		}
	}
	log.Printf("applied manage push: host=%s token=%q", cfg.Host, cfg.Token)
}
