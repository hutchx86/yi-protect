// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// UBNT L2 discovery protocol responder (UDP port 10001).
//
// Separate from the avclient WSS control channel: this is what makes an
// unadopted camera show up as discoverable/adoptable in Protect. Real cameras
// run it continuously via `discover`/`ubntbox`.
//
// Wire format (reverse-engineered from a live UVC G3 Flex reply): 4-byte
// header (version, command, big-endian body length) then TLVs (1-byte type,
// big-endian length, value):
//
//	0x02  MAC (6) + IPv4 (4)
//	0x01  MAC (6)
//	0x0a  uptime seconds, 4 bytes big-endian
//	0x0b  hostname
//	0x0c  product/model
//	0x03  firmware version
//	0x10  sysid, 2 bytes LITTLE-ENDIAN on the wire (verified against board.info)
//	0x17  is-managed flag, 4 bytes big-endian: 0 = adopted, nonzero = adoptable
//	0x20  stable device-id UUID (same value as the `device-id` WSS header)
//	0x26  controller/adoption UUID, 16 raw bytes, present only once adopted
//	0x2b  device GUID, 16 raw bytes (same value as `x-guid`)
//	0x2c  default-credentials flag, 1 byte (0x03 on real hardware)
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// isAdopted is set once the WSS control channel confirms adoption
// (ubnt_avclient_paramAgreement); the discovery responder reads it from its
// own goroutine. Persisted to disk (setAdopted/loadAdoptedState) so an
// already-adopted camera reports correctly from the first reply after a
// restart rather than looking adoptable until paramAgreement arrives.
var isAdopted atomic.Bool

// adoptedStateFilePath is where isAdopted is persisted. Set from the
// -adopted-state-file flag.
var adoptedStateFilePath = unifiPrefix + "/etc/unifi_client_go.adopted"

// setAdopted updates isAdopted and persists it. loadAdoptedState sets the
// in-memory flag directly to avoid rewriting the file.
func setAdopted(adopted bool) {
	isAdopted.Store(adopted)
	content := "false\n"
	if adopted {
		content = "true\n"
	}
	if err := os.WriteFile(adoptedStateFilePath, []byte(content), 0600); err != nil {
		log.Printf("setAdopted: failed to persist adopted=%v: %v", adopted, err)
	}
}

// loadAdoptedState reads the persisted adopted flag into isAdopted. A missing
// file (first-ever boot) leaves it false, which is correct.
func loadAdoptedState(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // file doesn't exist yet (first boot) or unreadable -- default false is correct
	}
	if strings.TrimSpace(string(data)) == "true" {
		isAdopted.Store(true)
	}
}

// adoptionUUID is our persisted adoption UUID (loadOrCreateAdoptionUUID),
// sent as the 0x26 TLV once adopted. Nil until main() sets it.
var adoptionUUID []byte

// deviceName is the camera's display name -- sent in hello/ChangeDeviceSettings/
// ChangeOsdSettings and as discovery's 0x0b hostname. Defaults to cfg.Model,
// then to whatever the Protect app sets. An atomic.Value so the responder
// reads renames immediately.
var deviceName atomic.Value // holds string

// deviceNameFilePath persists a controller-assigned name across restarts.
var deviceNameFilePath = unifiPrefix + "/etc/unifi_client_go.device-name"

// setDeviceName updates the live name and persists it.
func setDeviceName(name string) {
	deviceName.Store(name)
	if err := os.WriteFile(deviceNameFilePath, []byte(name), 0600); err != nil {
		log.Printf("setDeviceName: failed to persist name %q: %v", name, err)
	}
}

// getDeviceName returns the current name; safe from any goroutine.
func getDeviceName() string {
	if v, _ := deviceName.Load().(string); v != "" {
		return v
	}
	return cfg.Model // shouldn't normally happen -- loadDeviceName sets an initial value at startup
}

// loadDeviceName restores a persisted controller-assigned name, falling back
// to fallback (cfg.Model) on a first-ever boot.
func loadDeviceName(path, fallback string) {
	data, err := os.ReadFile(path)
	if err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			deviceName.Store(name)
			return
		}
	}
	deviceName.Store(fallback)
}

const discoveryPort = 10001

func macBytes(mac string) [6]byte {
	var out [6]byte
	mac = strings.ReplaceAll(mac, ":", "")
	for i := 0; i < 6 && i*2+2 <= len(mac); i++ {
		fmt.Sscanf(mac[i*2:i*2+2], "%02x", &out[i])
	}
	return out
}

func tlv(t byte, value []byte) []byte {
	out := make([]byte, 3+len(value))
	out[0] = t
	binary.BigEndian.PutUint16(out[1:3], uint16(len(value)))
	copy(out[3:], value)
	return out
}

func buildDiscoveryResponse(mac [6]byte, ip net.IP, hostname, product, fwVersion string, sysid uint16, uptime time.Duration, guid string) []byte {
	ip4 := ip.To4()
	var body []byte

	macIP := append(append([]byte{}, mac[:]...), ip4...)
	body = append(body, tlv(0x02, macIP)...)
	body = append(body, tlv(0x01, mac[:])...)

	up := make([]byte, 4)
	binary.BigEndian.PutUint32(up, uint32(uptime.Seconds()))
	body = append(body, tlv(0x0a, up)...)

	body = append(body, tlv(0x0b, []byte(hostname))...)
	body = append(body, tlv(0x0c, []byte(product))...)

	// 0x17 (is_managed): 0 = adopted, nonzero = adoptable. The controller's
	// own parser treats it as a boolean; 4 big-endian bytes is this device's
	// confirmed wire format.
	managed := make([]byte, 4)
	if !isAdopted.Load() {
		binary.BigEndian.PutUint32(managed, 1)
	}
	body = append(body, tlv(0x17, managed)...)

	body = append(body, tlv(0x03, []byte(fwVersion))...)

	if sysid != 0 {
		sid := make([]byte, 2)
		binary.LittleEndian.PutUint16(sid, sysid)
		body = append(body, tlv(0x10, sid)...)
	}

	// 0x20 (DEVICE_ID): stable per-device UUID, same value as the `device-id`
	// WSS header -- NOT the per-connection `x-guid` (that is 0x2b).
	if deviceIDStr != "" {
		body = append(body, tlv(0x20, []byte(deviceIDStr))...)
	}

	// 0x2b (GUID): 16 raw bytes, same value as `x-guid`.
	if guid != "" {
		if raw, err := parseUUID(guid); err == nil {
			body = append(body, tlv(0x2b, raw[:])...)
		}
	}

	// 0x2c (DEFAULT_CREDENTIALS): single byte, 0x03 on real hardware.
	body = append(body, tlv(0x2c, []byte{0x03})...)

	// 0x26 (adoption UUID): present only once adopted; an unadopted camera has
	// never persisted one.
	if isAdopted.Load() && len(adoptionUUID) == 16 {
		body = append(body, tlv(0x26, adoptionUUID)...)
	}

	header := make([]byte, 4)
	header[0] = 0x01 // version
	header[1] = 0x00 // command: response
	binary.BigEndian.PutUint16(header[2:4], uint16(len(body)))
	return append(header, body...)
}

// discoveryGroup is the multicast address real UniFi devices join. The
// controller probes both the limited-broadcast address and this group;
// broadcast is dropped by routers, so cross-subnet discovery only works for
// group members. Binding 0.0.0.0:10001 alone does NOT receive multicast.
var discoveryGroup = net.IPv4(233, 89, 188, 1)

// lastProbeNanos records when we last accepted a discovery request, used to
// log the first probe only once.
var lastProbeNanos atomic.Int64

// joinDiscoveryGroup adds this socket to the discovery multicast group on every
// usable interface, so routed probes arrive even on a dual-interface camera.
func joinDiscoveryGroup(conn *net.UDPConn) {
	g4 := discoveryGroup.To4()
	if g4 == nil {
		return
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return
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
			v4 := ipnet.IP.To4()
			if v4 == nil || v4.IsUnspecified() {
				continue
			}
			mreq := &syscall.IPMreq{}
			copy(mreq.Multiaddr[:], g4)
			copy(mreq.Interface[:], v4)
			if cerr := raw.Control(func(fd uintptr) {
				if err := syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq); err != nil {
					log.Printf("discovery: join %s on %s failed: %v", discoveryGroup, v4, err)
				} else {
					log.Printf("discovery: joined multicast %s on %s", discoveryGroup, v4)
				}
			}); cerr != nil {
				log.Printf("discovery: membership control on %s failed: %v", v4, cerr)
			}
			break // one IPv4 per interface is enough
		}
	}
}

// runDiscoveryResponder listens for UBNT discovery broadcast/unicast/multicast
// queries on UDP 10001 and replies. Runs independently of the avclient WSS
// connection state -- real cameras respond whether adopted or not.
func runDiscoveryResponder(mac [6]byte, ip net.IP, product, fwVersion string, sysid uint16, guid string) {
	addr := &net.UDPAddr{Port: discoveryPort, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("discovery: failed to bind :%d: %v", discoveryPort, err)
		return
	}
	defer conn.Close()
	joinDiscoveryGroup(conn)
	log.Printf("discovery: listening on :%d", discoveryPort)

	start := time.Now()
	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("discovery: read error: %v", err)
			continue
		}
		if n < 4 || buf[0] != 0x01 || buf[1] != 0x00 {
			continue // not a v1 discovery request we understand
		}
		if lastProbeNanos.Load() == 0 {
			log.Printf("discovery: first probe received from %s -- a controller can see us", src)
		}
		lastProbeNanos.Store(time.Now().UnixNano())
		// Read the name fresh on each reply so a controller-pushed rename
		// takes effect without restarting the responder.
		resp := buildDiscoveryResponse(mac, ip, getDeviceName(), product, fwVersion, sysid, time.Since(start), guid)
		if _, err := conn.WriteToUDP(resp, src); err != nil {
			log.Printf("discovery: reply to %s failed: %v", src, err)
		}
	}
}
