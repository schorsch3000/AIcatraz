// SPDX-License-Identifier: GPL-3.0-only

package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Session is one recorded agent session of a project.
type Session struct {
	Tool      string    `json:"tool"`
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	ExitCode  int       `json:"exit_code"`
	Image     string    `json:"image,omitempty"`
	Directory string    `json:"directory,omitempty"`
}

const maxSessions = 200

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// ValidID reports whether id is safe to pass to an agent as one argument. Ids
// come from files the agent can write, so they are never trusted.
func ValidID(id string) bool { return sessionID.MatchString(id) }

// NewSessionID returns a random UUID (version 4).
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Store keeps the session index in the project's state directory, outside the
// directories mounted into the jail, so the agent can't edit it.
type Store struct {
	path string
	mu   sync.Mutex
}

func OpenStore(stateDir string) *Store { return &Store{path: filepath.Join(stateDir, "sessions.json")} }

func (s *Store) load() ([]Session, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return doc.Sessions, nil
}

// List returns the sessions, most recent first.
func (s *Store) List() ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Ended.After(list[j].Ended) })
	return list, nil
}

// Add records a session, replacing an earlier record with the same tool and id.
func (s *Store) Add(n Session) error {
	if !ValidID(n.ID) {
		return fmt.Errorf("invalid session id %q", n.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	kept := list[:0]
	for _, x := range list {
		if !(x.Tool == n.Tool && x.ID == n.ID) {
			kept = append(kept, x)
		}
	}
	list = append(kept, n)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Ended.Before(list[j].Ended) })
	if len(list) > maxSessions {
		list = list[len(list)-maxSessions:]
	}
	data, err := json.MarshalIndent(map[string]any{"sessions": list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Find returns the session with this id (any tool), or nil.
func (s *Store) Find(id string) (*Session, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, nil
}

// ClaudeSessionExists reports whether Claude Code wrote a transcript for id in
// its config directory. A session without any message leaves no transcript and
// can't be resumed, so it isn't worth recording.
func ClaudeSessionExists(configDir, id string) bool {
	if !ValidID(id) {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", id+".jsonl"))
	return len(matches) > 0
}

// OpencodeSession is one entry of `opencode session list --format json`.
type OpencodeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"` // milliseconds since the epoch
	Created   int64  `json:"created"`
	Directory string `json:"directory"`
}

// ParseOpencodeSessions parses the JSON list.
func ParseOpencodeSessions(data []byte) ([]OpencodeSession, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil // opencode prints nothing at all when there are no sessions
	}
	var list []OpencodeSession
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("opencode session list: %w", err)
	}
	return list, nil
}

// PickOpencodeSession chooses the session a run just produced: the most
// recently updated one in dir that was touched since the run began.
func PickOpencodeSession(list []OpencodeSession, dir string, since time.Time) (OpencodeSession, bool) {
	var best OpencodeSession
	found := false
	for _, s := range list {
		if s.Directory != dir || !ValidID(s.ID) || time.UnixMilli(s.Updated).Before(since) {
			continue
		}
		if !found || s.Updated > best.Updated {
			best, found = s, true
		}
	}
	return best, found
}
