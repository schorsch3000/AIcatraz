package proto

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func pair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return New(a), New(b)
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := pair(t)
	go func() {
		a.Write(TStdin, []byte("hello"))
		a.WriteJSON(THello, Hello{Version: Version, Name: "make", Args: []string{"-j", "4"}})
		a.Write(TPing, nil)
	}()
	typ, p, err := b.Read()
	if err != nil || typ != TStdin || string(p) != "hello" {
		t.Fatalf("got %q %q %v", typ, p, err)
	}
	var h Hello
	if err := b.ReadJSON(THello, &h); err != nil || h.Name != "make" || len(h.Args) != 2 {
		t.Fatalf("hello: %+v %v", h, err)
	}
	if typ, p, err = b.Read(); err != nil || typ != TPing || len(p) != 0 {
		t.Fatalf("ping: %q %q %v", typ, p, err)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	a, _ := pair(t)
	if err := a.Write(TStdin, make([]byte, MaxFrame+1)); err == nil {
		t.Error("writing an oversized frame must fail")
	}
	// A peer announcing a huge length must not make the reader allocate it.
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	go x.Write([]byte{byte(TStdin), 0xff, 0xff, 0xff, 0xff})
	if _, _, err := New(y).Read(); err == nil {
		t.Error("oversized announced frame must fail")
	}
}

func TestReadTimesOutOnSilence(t *testing.T) {
	old := Timeout
	Timeout = 50 * time.Millisecond
	defer func() { Timeout = old }()
	_, b := pair(t)
	_, _, err := b.Read()
	if !IsTimeout(err) {
		t.Errorf("want a timeout, got %v", err)
	}
}

func TestKeepAlive(t *testing.T) {
	oi := Interval
	Interval = 10 * time.Millisecond
	defer func() { Interval = oi }()
	a, b := pair(t)
	stop := make(chan struct{})
	go a.KeepAlive(stop)
	defer close(stop)
	typ, _, err := b.Read()
	if err != nil || typ != TPing {
		t.Errorf("got %q %v", typ, err)
	}
}

func TestResize(t *testing.T) {
	r, c, ok := ParseResize(ResizePayload(24, 80))
	if !ok || r != 24 || c != 80 {
		t.Errorf("%d %d %v", r, c, ok)
	}
	if _, _, ok := ParseResize([]byte{1}); ok {
		t.Error("short payload must not parse")
	}
	if !bytes.Equal(ResizePayload(1, 2), []byte{0, 1, 0, 2}) {
		t.Error("encoding")
	}
}
