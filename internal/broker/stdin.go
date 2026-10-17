// SPDX-License-Identifier: GPL-3.0-only

package broker

import (
	"io"
	"sync"

	"github.com/schorsch3000/aicatraz/internal/proto"
)

// stdinQueue passes the shim's stdin to the command without ever blocking the
// goroutine that reads the connection. That goroutine also has to notice a
// disconnect, a signal to forward or a heartbeat, and a command that doesn't
// read its stdin (or reads it slowly) would otherwise freeze it.
//
// The shim may only send as much as it was granted (proto.StdinWindow, topped
// up here as the command consumes data), so the queue is bounded; a shim that
// exceeds its credit is misbehaving and the caller ends the session.
type stdinQueue struct {
	conn *proto.Conn
	dst  io.WriteCloser

	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	queued int
	eof    bool
	stop   bool
}

func newStdinQueue(conn *proto.Conn, dst io.WriteCloser) *stdinQueue {
	q := &stdinQueue{conn: conn, dst: dst}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// start grants the initial credit and begins writing to the command.
func (q *stdinQueue) start() {
	_ = q.conn.Write(proto.TWindow, proto.WindowPayload(proto.StdinWindow))
	go q.run()
}

// push queues data. It returns false if the shim sent more than it had credit for.
func (q *stdinQueue) push(p []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued+len(p) > proto.StdinWindow+proto.ChunkSize {
		return false
	}
	q.chunks = append(q.chunks, append([]byte(nil), p...))
	q.queued += len(p)
	q.cond.Signal()
	return true
}

// end marks the end of stdin; the command's stdin is closed once the queue drains.
func (q *stdinQueue) end() {
	q.mu.Lock()
	q.eof = true
	q.mu.Unlock()
	q.cond.Signal()
}

// close stops the writer (the command is gone or the session is over).
func (q *stdinQueue) close() {
	q.mu.Lock()
	q.stop = true
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *stdinQueue) run() {
	for {
		q.mu.Lock()
		for len(q.chunks) == 0 && !q.eof && !q.stop {
			q.cond.Wait()
		}
		if q.stop {
			q.mu.Unlock()
			return
		}
		if len(q.chunks) == 0 { // eof and drained
			q.mu.Unlock()
			_ = q.dst.Close()
			return
		}
		chunk := q.chunks[0]
		q.chunks = q.chunks[1:]
		q.mu.Unlock()

		// May block for as long as the command doesn't read; nothing else waits on it.
		if _, err := q.dst.Write(chunk); err != nil {
			return // the command has gone away
		}
		q.mu.Lock()
		q.queued -= len(chunk)
		q.mu.Unlock()
		_ = q.conn.Write(proto.TWindow, proto.WindowPayload(uint32(len(chunk))))
	}
}
