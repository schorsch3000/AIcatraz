package proto

import (
	"bytes"
	"net"
	"sync"
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

func TestCredit(t *testing.T) {
	c := NewCredit()
	got := make(chan int, 1)
	go func() { got <- c.Take(100) }()
	select {
	case <-got:
		t.Fatal("Take must wait until there is credit")
	case <-time.After(50 * time.Millisecond):
	}
	c.Add(30)
	if n := <-got; n != 30 {
		t.Errorf("took %d, want 30", n)
	}
	c.Add(500)
	if n := c.Take(100); n != 100 {
		t.Errorf("take capped at max: %d", n)
	}
	if n := c.Take(1000); n != 400 {
		t.Errorf("the rest: %d", n)
	}
	// Close releases waiters.
	go func() { time.Sleep(20 * time.Millisecond); c.Close() }()
	if n := c.Take(10); n != 0 {
		t.Errorf("closed credit gives 0, got %d", n)
	}
}

func TestWindowPayload(t *testing.T) {
	if n, ok := ParseWindow(WindowPayload(262144)); !ok || n != 262144 {
		t.Errorf("%d %v", n, ok)
	}
	if _, ok := ParseWindow([]byte{1, 2}); ok {
		t.Error("short payload")
	}
}

func TestWriteWaitsForASlowButLivePeer(t *testing.T) {
	old := Timeout
	Timeout = 100 * time.Millisecond
	a, b := pair(t)
	var wg sync.WaitGroup
	defer func() {
		a.Close()
		b.Close()
		wg.Wait() // no goroutine may still read Timeout when it is restored
		Timeout = old
	}()

	// b keeps pinging a (so a knows b is alive) but doesn't read for a while.
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if b.Write(TPing, nil) != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		for { // a reads b's pings, which marks b as alive
			if _, _, err := a.Read(); err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	wg.Add(1)
	go func() { defer wg.Done(); done <- a.Write(TStdout, make([]byte, 64<<10)) }()
	time.Sleep(450 * time.Millisecond) // several times the timeout, with nobody reading on b
	select {
	case err := <-done:
		t.Fatalf("a write to a live-but-slow peer must wait, got %v", err)
	default:
	}
	// Now b reads the pings out of the way and then the data.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, _, err := b.Read(); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write failed after the peer caught up: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("write did not complete")
	}
	close(stop)
}

func TestWriteFailsForADeadPeer(t *testing.T) {
	old := Timeout
	Timeout = 100 * time.Millisecond
	defer func() { Timeout = old }()
	a, _ := pair(t) // the other end never reads or writes
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	if err := a.Write(TStdout, make([]byte, 64<<10)); err == nil {
		t.Error("a silent peer must make the write fail")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
