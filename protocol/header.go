package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Stream header layout:
//
//	byte 0      magic 'C'
//	byte 1      header version (1)
//	bytes 2..3  big-endian length N of the JSON body (1..MaxHeaderBody)
//	bytes 4..   JSON body (StreamHeader)
const (
	headerMagic   = 'C'
	headerVersion = 1
	// MaxHeaderBody bounds what a peer can make us allocate before any validation.
	MaxHeaderBody = 4096
)

var (
	ErrBadMagic   = errors.New("protocol: bad stream header magic")
	ErrBadVersion = errors.New("protocol: unsupported stream header version")
	ErrBadLength  = errors.New("protocol: bad stream header length")
)

// StreamHeader tells the agent which application a stream is for. The agent maps the hostname to
// an upstream using its own local ingress rules, so the edge can never pick an arbitrary upstream
// inside the customer's network.
//
// Kind is empty for a proxied connection. KindHealth asks for origin health (P3-12); the edge only
// sends it to agents that announced FeatureHealth, because older agents reject unknown fields.
type StreamHeader struct {
	Hostname string `json:"host,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// Validate checks the header is safe to use as a lookup key.
func (h StreamHeader) Validate() error {
	switch h.Kind {
	case "":
		return ValidateHostname(h.Hostname)
	case KindHealth:
		if h.Hostname != "" {
			return errors.New("protocol: a health stream has no hostname")
		}
		return nil
	default:
		return fmt.Errorf("protocol: unknown stream kind %q", h.Kind)
	}
}

// ValidateHostname accepts lowercase DNS names (letters, digits, '-', '.'), max 253 characters.
// Matching is exact, so callers must lowercase and strip ports first.
func ValidateHostname(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("protocol: invalid hostname length %d", len(host))
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("protocol: invalid hostname %q", host)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("protocol: invalid hostname %q", host)
			}
		}
	}
	return nil
}

// WriteStreamHeader writes h as the first bytes of a stream.
func WriteStreamHeader(w io.Writer, h StreamHeader) error {
	if err := h.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(body) > MaxHeaderBody {
		return ErrBadLength
	}
	buf := make([]byte, 4+len(body))
	buf[0], buf[1] = headerMagic, headerVersion
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// ReadStreamHeader reads and validates a stream header. It reads exactly the header bytes, so the
// reader is positioned at the first byte of the HTTP payload afterwards.
func ReadStreamHeader(r io.Reader) (StreamHeader, error) {
	var h StreamHeader
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return h, err
	}
	if prefix[0] != headerMagic {
		return h, ErrBadMagic
	}
	if prefix[1] != headerVersion {
		return h, ErrBadVersion
	}
	n := int(binary.BigEndian.Uint16(prefix[2:4]))
	if n == 0 || n > MaxHeaderBody {
		return h, ErrBadLength
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return h, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return StreamHeader{}, fmt.Errorf("protocol: decode stream header: %w", err)
	}
	if dec.More() {
		return StreamHeader{}, errors.New("protocol: trailing data in stream header")
	}
	if err := h.Validate(); err != nil {
		return StreamHeader{}, err
	}
	return h, nil
}
