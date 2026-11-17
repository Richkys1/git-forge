// pkt-line framing (git smart HTTP) and sideband (band 1 = data, 2 = progress, 3 = error).
// A pkt-line is 4 hex digits of total length (payload + 4) + payload. 0000 = flush,
// 0001 = delim, 0002 = response-end. Max packet 65520 bytes.
// Faithful port of lib/pkt.ts.
package main

import (
	"bytes"
	"errors"
	"strings"
)

const maxPkt = 65520

// Encode one payload as a pkt-line: one allocation (header + body in a single buffer).
func pkt(payload []byte) []byte {
	length := len(payload) + 4
	if length > maxPkt {
		panic("pkt-line too large")
	}
	out := make([]byte, length)
	out[0] = hexDigitUpper(byte(length>>12) & 0xf)
	out[1] = hexDigitUpper(byte(length>>8) & 0xf)
	out[2] = hexDigitUpper(byte(length>>4) & 0xf)
	out[3] = hexDigitUpper(byte(length) & 0xf)
	copy(out[4:], payload)
	return out
}

func pktStr(s string) []byte { return pkt([]byte(s)) }

func hexDigitUpper(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}

// A flush packet.
var flushPkt = []byte("0000")

// A delim packet (0001).
var delimPkt = []byte("0001")

// A response-end packet (0002).
var respEndPkt = []byte("0002")

// A sideband packet for band 1/2/3. Payloads over 65516 bytes are split.
func sideband(band byte, data []byte) [][]byte {
	var out [][]byte
	offset := 0
	for {
		end := offset + maxPkt - 5
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		body := make([]byte, 0, len(chunk)+1)
		body = append(body, band)
		body = append(body, chunk...)
		out = append(out, pkt(body))
		offset = end
		if offset >= len(data) {
			break
		}
	}
	return out
}

// Value of one hex digit, or -1 when the byte is not a hex digit.
func hexNibble(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

// Read a 4-hex-digit pkt-line length at `offset`, or -1 when those bytes are not
// four hex digits.
func pktLineLength(buf []byte, offset int) int {
	if offset+4 > len(buf) {
		return -1
	}
	a := hexNibble(buf[offset])
	b := hexNibble(buf[offset+1])
	c := hexNibble(buf[offset+2])
	d := hexNibble(buf[offset+3])
	if a < 0 || b < 0 || c < 0 || d < 0 {
		return -1
	}
	return (a << 12) | (b << 8) | (c << 4) | d
}

type PktFrameType int

const (
	FrameData PktFrameType = iota
	FrameFlush
	FrameDelim
	FrameRespEnd
)

type PktFrame struct {
	Type PktFrameType
	Data []byte
}

// Parse a byte stream into pkt-line frames.
func parsePktLines(buf []byte) []PktFrame {
	frames, _ := splitFramesAndRest(buf)
	return frames
}

// Split a byte buffer into the frames after a leading pack of raw bytes (used for push requests).
func splitFramesAndRest(buf []byte) ([]PktFrame, int) {
	var frames []PktFrame
	offset := 0
	for offset+4 <= len(buf) {
		length := pktLineLength(buf, offset)
		if length < 0 {
			break
		}
		switch length {
		case 0:
			frames = append(frames, PktFrame{Type: FrameFlush})
			offset += 4
			continue
		case 1:
			frames = append(frames, PktFrame{Type: FrameDelim})
			offset += 4
			continue
		case 2:
			frames = append(frames, PktFrame{Type: FrameRespEnd})
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(buf) {
			break
		}
		frames = append(frames, PktFrame{Type: FrameData, Data: buf[offset+4 : offset+length]})
		offset += length
	}
	return frames, offset
}

// Parse a capability line "key" or "key=value" (space separated after the first \0 on a ref line).
func parseCapabilities(text string) map[string]string {
	caps := map[string]string{}
	for _, token := range strings.Split(text, " ") {
		if token == "" {
			continue
		}
		if eq := strings.Index(token, "="); eq == -1 {
			caps[token] = ""
		} else {
			caps[token[:eq]] = token[eq+1:]
		}
	}
	return caps
}

var errPktTooLarge = errors.New("pkt-line too large")

var _ = bytes.Equal
