// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// termLog records what a client sent to the terminal ('>') and what the
// terminal sent in ('<'), one quoted line each, with the time. It is for
// finding out what a program does to a terminal. It contains everything that
// was shown and typed, passwords and keys included.
type termLog struct {
	mu sync.Mutex
	f  *os.File
}

func openTermLog(path string) (*termLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &termLog{f: f}, nil
}

// write records bytes; a nil log does nothing.
func (l *termLog) write(dir byte, b []byte) {
	if l == nil || len(b) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s %c %s\n", time.Now().Format("15:04:05.000"), dir, strconv.Quote(string(b)))
}

// Close ends the record.
func (l *termLog) Close() {
	if l != nil {
		l.f.Close()
	}
}
