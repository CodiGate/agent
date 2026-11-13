package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStreamHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := StreamHeader{Hostname: "app.example.com"}
	if err := WriteStreamHeader(&buf, in); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("GET / HTTP/1.1\r\n")
	out, err := ReadStreamHeader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	if rest, _ := io.ReadAll(&buf); string(rest) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("header read consumed payload bytes, rest=%q", rest)
	}
}

func TestReadStreamHeaderRejects(t *testing.T) {
	cases := map[string]struct {
		in   []byte
		want error
	}{
		"bad magic":    {[]byte{'X', 1, 0, 2, '{', '}'}, ErrBadMagic},
		"bad version":  {[]byte{'C', 9, 0, 2, '{', '}'}, ErrBadVersion},
		"zero length":  {[]byte{'C', 1, 0, 0}, ErrBadLength},
		"huge length":  {[]byte{'C', 1, 0xff, 0xff}, ErrBadLength},
		"short body":   {[]byte{'C', 1, 0, 10, '{'}, io.ErrUnexpectedEOF},
		"empty stream": {nil, io.EOF},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadStreamHeader(bytes.NewReader(tc.in))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadStreamHeaderRejectsBadBodies(t *testing.T) {
	for _, body := range []string{
		`{"host":"UPPER.example.com"}`,
		`{"host":"evil.com:22"}`,
		`{"host":"a..b"}`,
		`{"host":""}`,
		`{"host":"ok.example.com","upstream":"http://10.0.0.1"}`,
		`{"host":"a.com"}{"host":"b.com"}`,
		`not json`,
	} {
		b := append([]byte{'C', 1, 0, byte(len(body))}, body...)
		if _, err := ReadStreamHeader(bytes.NewReader(b)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestValidateHostname(t *testing.T) {
	good := []string{"a", "app.example.com", "x-1.codigate.nl", strings.Repeat("a", 63) + ".eu"}
	bad := []string{"", "-a.com", "a-.com", "a_b.com", "a.com.", strings.Repeat("a", 64) + ".eu", "ä.com", "a b"}
	for _, h := range good {
		if err := ValidateHostname(h); err != nil {
			t.Errorf("%q: %v", h, err)
		}
	}
	for _, h := range bad {
		if ValidateHostname(h) == nil {
			t.Errorf("%q accepted", h)
		}
	}
}

func FuzzReadStreamHeader(f *testing.F) {
	var seed bytes.Buffer
	_ = WriteStreamHeader(&seed, StreamHeader{Hostname: "app.example.com"})
	f.Add(seed.Bytes())
	f.Add([]byte{'C', 1, 0, 2, '{', '}'})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ReadStreamHeader(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Anything accepted must be valid and survive a round trip unchanged.
		if h.Validate() != nil {
			t.Fatalf("accepted invalid header %+v", h)
		}
		var buf bytes.Buffer
		if err := WriteStreamHeader(&buf, h); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		h2, err := ReadStreamHeader(&buf)
		if err != nil || h2 != h {
			t.Fatalf("round trip: %+v %v", h2, err)
		}
	})
}
