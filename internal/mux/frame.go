// SPDX-License-Identifier: GPL-3.0-only

// Package mux is a small terminal multiplexer: it runs one program on a
// pseudo-terminal, keeps it alive when nobody is attached, and lets a client
// attach and detach. It also owns the approval prompt: a request from the
// jail is drawn by the client on the real terminal, over whatever the program
// shows, and the keys that answer it never reach the program.
package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame types. Client to server:
const (
	fHello  = 'H' // rows u16, cols u16: attach
	fInput  = 'I' // bytes typed by the user
	fResize = 'R' // rows u16, cols u16
	fDetach = 'D' // the user detached
	fAnswer = 'A' // id u32, answer text: the user answered a prompt
	fRedraw = 'W' // ask the program to repaint
	fStop   = 'S' // first frame instead of fHello: end the session
)

// Server to client:
const (
	fOutput   = 'O' // bytes the program wrote
	fPreamble = 'P' // terminal modes to set so the terminal matches the program's
	fPrompt   = 'Q' // id u32, timeout ms u32, request JSON
	fCancel   = 'C' // id u32: the prompt is gone
	fInfo     = 'N' // started u64 (unix seconds), label: what the status bar shows
	fExit     = 'X' // exit code i32: the program ended
	fError    = 'E' // text: the server is closing this connection
)

const maxFrame = 1 << 20

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errors.New("mux: frame too large")
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf) // one write, so frames from different goroutines can't interleave under a lock
	return err
}

func readFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("mux: frame of %d bytes", n)
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func sizePayload(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b
}

func parseSize(p []byte) (rows, cols uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(p[0:2]), binary.BigEndian.Uint16(p[2:4]), true
}
