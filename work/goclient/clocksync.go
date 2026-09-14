// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// Go port of unifi-cam-proxy's clock_sync.py. UniFi Protect's avclient video
// ingest ("extendedFlv") isn't plain FLV: every tag gets a 16-byte proprietary
// trailer, and periodic onClockSync/onMpma AMF0 script tags are injected.
// Reimplemented in Go because the camera runs MicroPython, not CPython.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"time"
)

// writeTimestampTrailer appends the 16-byte trailer after every tag: one zero
// byte, an 11-byte marker (video vs other), then a big-endian elapsed-time
// value in 1/100000 s since stream start.
func writeTimestampTrailer(w io.Writer, isVideoTag bool, elapsed time.Duration) error {
	buf := make([]byte, 0, 16)
	buf = append(buf, 0x00)
	if isVideoTag {
		buf = append(buf, 1, 95, 144, 0, 0, 0, 0, 0, 0, 0, 0)
	} else {
		buf = append(buf, 0, 43, 17, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	ticks := uint32(elapsed.Seconds() * 1000 * 100)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint32(tail, ticks)
	buf = append(buf, tail...)
	_, err := w.Write(buf)
	return err
}

func amf0Key(buf []byte, s string) []byte {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(s)))
	return append(buf, s...)
}

func amf0String(buf []byte, s string) []byte {
	buf = append(buf, 0x02)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(s)))
	return append(buf, s...)
}

func amf0Number(buf []byte, v float64) []byte {
	buf = append(buf, 0x00)
	return binary.BigEndian.AppendUint64(buf, math.Float64bits(v))
}

// buildScriptTag builds a complete FLV script tag (11-byte header + AMF0
// payload): an AMF0 string name followed by an AMF0 Object of props.
func buildScriptTag(name string, props []amfProp, timestampMs uint32) []byte {
	var data []byte
	data = amf0String(data, name)
	data = append(data, 0x03) // AMF0 Object marker
	for _, p := range props {
		data = amf0Key(data, p.key)
		switch v := p.val.(type) {
		case float64:
			data = amf0Number(data, v)
		case string:
			data = amf0String(data, v)
		case []amfProp:
			data = append(data, 0x03)
			for _, sub := range v {
				data = amf0Key(data, sub.key)
				data = amf0Number(data, sub.val.(float64))
			}
			data = binary.BigEndian.AppendUint16(data, 0)
			data = append(data, 0x09)
		}
	}
	data = binary.BigEndian.AppendUint16(data, 0)
	data = append(data, 0x09) // object end marker

	tag := make([]byte, 0, 11+len(data))
	tag = append(tag, 0x12) // script tag type
	tag = append(tag, byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
	tag = append(tag, byte(timestampMs>>16), byte(timestampMs>>8), byte(timestampMs), byte(timestampMs>>24))
	tag = append(tag, 0, 0, 0) // stream ID
	tag = append(tag, data...)
	return tag
}

type amfProp struct {
	key string
	val interface{}
}

// clockSyncCopy reads an FLV stream from r and writes the UniFi-flavored
// version to w: flags forced to 0x07, a 16-byte trailer after every tag, and
// onClockSync/onMpma tags injected roughly every 5 s. Returns on EOF or a
// write failure.
func clockSyncCopy(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 32*1024)
	start := time.Now()

	header := make([]byte, 9)
	if _, err := io.ReadFull(br, header); err != nil {
		return fmt.Errorf("read FLV header after %s: %w", time.Since(start), err)
	}
	log.Printf("clockSyncCopy: got FLV header after %s", time.Since(start))
	header[4] = 0x07 // clock_sync.py hardcodes this flags byte
	if _, err := w.Write(header); err != nil {
		return err
	}
	prevTagSize0 := make([]byte, 4)
	if _, err := io.ReadFull(br, prevTagSize0); err != nil {
		return fmt.Errorf("read PreviousTagSize0: %w", err)
	}
	if _, err := w.Write(prevTagSize0); err != nil {
		return err
	}

	lastSync := time.Now()
	for {
		tagHeader := make([]byte, 11)
		if _, err := io.ReadFull(br, tagHeader); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read tag header: %w", err)
		}
		tagType := tagHeader[0]
		dataSize := int(tagHeader[1])<<16 | int(tagHeader[2])<<8 | int(tagHeader[3])
		tsBytes := []byte{tagHeader[7], tagHeader[4], tagHeader[5], tagHeader[6]}
		timestampMs := binary.BigEndian.Uint32(tsBytes)

		now := time.Now()
		if now.Sub(lastSync) >= 5*time.Second {
			lastSync = now
			elapsed := now.Sub(start)

			syncTag := buildScriptTag("onClockSync", []amfProp{
				{"streamClock", float64(timestampMs)},
				{"streamClockBase", float64(0)},
				{"wallClock", float64(now.UnixMilli())},
			}, timestampMs)
			if _, err := w.Write(syncTag); err != nil {
				return err
			}
			if err := writeTimestampTrailer(w, false, elapsed); err != nil {
				return err
			}

			mpmaTag := buildScriptTag("onMpma", []amfProp{
				{"cs", []amfProp{{"cur", 1500000.0}, {"max", 1500000.0}, {"min", 1500000.0}}},
				{"m", []amfProp{{"cur", 1500000.0}, {"max", 1500000.0}, {"min", 1500000.0}}},
				{"r", float64(0)},
				{"sp", []amfProp{{"cur", 1500000.0}, {"max", 1500000.0}, {"min", 150000.0}}},
				{"t", 75000.0},
			}, 0)
			if _, err := w.Write(mpmaTag); err != nil {
				return err
			}
			if err := writeTimestampTrailer(w, false, elapsed); err != nil {
				return err
			}
		}

		if _, err := w.Write(tagHeader); err != nil {
			return err
		}
		data := make([]byte, dataSize)
		if _, err := io.ReadFull(br, data); err != nil {
			return fmt.Errorf("read tag data: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		prevTagSize := make([]byte, 4)
		if _, err := io.ReadFull(br, prevTagSize); err != nil {
			return fmt.Errorf("read PreviousTagSize: %w", err)
		}
		if _, err := w.Write(prevTagSize); err != nil {
			return err
		}

		if err := writeTimestampTrailer(w, tagType == 9, time.Since(start)); err != nil {
			return err
		}
	}
}
