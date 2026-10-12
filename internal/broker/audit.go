package broker

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Entry is one audit log line. Environment values are never logged, only the
// names of the variables handed to the command.
type Entry struct {
	Time       time.Time `json:"time"`
	Session    string    `json:"session,omitempty"`
	Event      string    `json:"event"` // "request", "ask" or "exit"
	PeerPID    int       `json:"peer_pid,omitempty"`
	Name       string    `json:"name,omitempty"`
	Args       []string  `json:"args,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Rule       *int      `json:"rule,omitempty"` // index of the matching rule; absent if none matched
	Decision   string    `json:"decision,omitempty"`
	Message    string    `json:"message,omitempty"`
	DecidedBy  string    `json:"decided_by,omitempty"` // "human", "session-grant", "timeout", "cancelled"
	Binary     string    `json:"binary,omitempty"`
	EnvPassed  []string  `json:"env_passed,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Signal     int       `json:"signal,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

// Audit appends JSON lines to a file the container can't reach.
type Audit struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// OpenAudit opens (creating if needed) the log in append mode.
func OpenAudit(path string) (*Audit, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f, enc: json.NewEncoder(f)}, nil
}

// Log writes one entry. A nil *Audit discards entries.
func (a *Audit) Log(e Entry) error {
	if a == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enc.Encode(e)
}

func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	return a.f.Close()
}
