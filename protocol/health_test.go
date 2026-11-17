package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestHealthStreamHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStreamHeader(&buf, StreamHeader{Kind: KindHealth}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `{"kind":"health"}`) {
		t.Fatalf("unexpected body %q", buf.String())
	}
	h, err := ReadStreamHeader(&buf)
	if err != nil || h.Kind != KindHealth || h.Hostname != "" {
		t.Fatalf("got %+v, %v", h, err)
	}
}

func TestProxyHeaderUnchangedOnTheWire(t *testing.T) {
	// Older agents decode with DisallowUnknownFields: a proxied stream must still be {"host":...}.
	var buf bytes.Buffer
	if err := WriteStreamHeader(&buf, StreamHeader{Hostname: "a.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String()[4:]; got != `{"host":"a.example.com"}` {
		t.Fatalf("wire body changed: %q", got)
	}
}

func TestStreamHeaderKindValidation(t *testing.T) {
	bad := []StreamHeader{
		{},
		{Kind: "health", Hostname: "a.example.com"},
		{Kind: "metrics"},
	}
	for _, h := range bad {
		if err := h.Validate(); err == nil {
			t.Errorf("%+v: want error", h)
		}
	}
	raw := []byte(`{"kind":"other"}`)
	in := append([]byte{'C', 1, 0, byte(len(raw))}, raw...)
	if _, err := ReadStreamHeader(bytes.NewReader(in)); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestHealthMessages(t *testing.T) {
	req, err := ReadHealthRequest(strings.NewReader(`{"hosts":["a.example.com","b.example.com"]}`))
	if err != nil || len(req.Hosts) != 2 {
		t.Fatalf("got %+v, %v", req, err)
	}
	if _, err := ReadHealthRequest(strings.NewReader(`{"hosts":["Bad Host"]}`)); err == nil {
		t.Fatal("bad host accepted")
	}
	if _, err := ReadHealthResponse(strings.NewReader(`{"results":[{"host":"a.example.com","status":"sideways"}]}`)); err == nil {
		t.Fatal("bad status accepted")
	}
	long := strings.Repeat("x", MaxHealthError+1)
	if _, err := ReadHealthResponse(strings.NewReader(`{"results":[{"host":"a.example.com","status":"down","error":"` + long + `"}]}`)); err == nil {
		t.Fatal("long error accepted")
	}
	resp, err := ReadHealthResponse(strings.NewReader(`{"results":[{"host":"a.example.com","status":"up"}]}`))
	if err != nil || resp.Results[0].Status != HealthUp {
		t.Fatalf("got %+v, %v", resp, err)
	}
}

func TestTruncateHealthError(t *testing.T) {
	s := strings.Repeat("é", 150)
	got := TruncateHealthError(s)
	if len(got) > MaxHealthError || !strings.HasPrefix(s, got) || len(got)%2 != 0 {
		t.Fatalf("bad truncation, len %d", len(got))
	}
}
