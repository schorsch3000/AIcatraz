package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store keeps the rules the human chose to remember, in a file in the
// project's state directory (outside every mount). They come after the rules
// of the config, so a deny in the config always wins.
type Store struct {
	path string
	mu   sync.Mutex
}

// OpenStore returns the store at path; the file is created on the first Add.
func OpenStore(path string) *Store { return &Store{path: path} }

// Path is the file the rules are kept in.
func (s *Store) Path() string { return s.path }

type storedRule struct {
	To       string `json:"to"`
	Ports    string `json:"ports,omitempty"`
	Proto    string `json:"proto,omitempty"`
	Decision string `json:"decision"`
}

type storeFile struct {
	Rules []storedRule `json:"rules"`
}

func (s *Store) read() (storeFile, error) {
	var f storeFile
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.path, err)
	}
	return f, nil
}

func (s *Store) write(f storeFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".net-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func toRule(r storedRule) (Rule, error) {
	t, err := ParseTarget(r.To)
	if err != nil {
		return Rule{}, err
	}
	rule := Rule{Target: t, Decision: Decision(r.Decision)}
	switch rule.Decision {
	case Allow, Ask, Deny:
	default:
		return Rule{}, fmt.Errorf("decision %q", r.Decision)
	}
	if r.Ports != "" {
		if rule.Ports, err = ParsePorts(r.Ports); err != nil {
			return Rule{}, err
		}
	}
	if r.Proto != "" {
		if rule.Protos, err = ParseProtos([]string{r.Proto}); err != nil {
			return Rule{}, err
		}
	}
	return rule, nil
}

func fromRule(r Rule) storedRule {
	sr := storedRule{To: r.Target.String(), Decision: string(r.Decision)}
	if !r.Ports.IsEmpty() {
		sr.Ports = r.Ports.String()
	}
	if len(r.Protos) > 0 {
		ps := make([]string, len(r.Protos))
		for i, p := range r.Protos {
			ps[i] = string(p)
		}
		sr.Proto = strings.Join(ps, ",")
	}
	return sr
}

// Rules returns the remembered rules in the order they were added. Entries
// that no longer parse (an edited file) are skipped.
func (s *Store) Rules() ([]Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	var rules []Rule
	for _, sr := range f.Rules {
		if r, err := toRule(sr); err == nil {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// Add remembers a rule, unless the same one is there already.
func (s *Store) Add(r Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	sr := fromRule(r)
	for _, have := range f.Rules {
		if have == sr {
			return nil
		}
	}
	f.Rules = append(f.Rules, sr)
	return s.write(f)
}

// Forget removes the rule at index i of Rules (counting from 0).
func (s *Store) Forget(i int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	if i < 0 || i >= len(f.Rules) {
		return fmt.Errorf("no remembered rule number %d", i+1)
	}
	f.Rules = append(f.Rules[:i], f.Rules[i+1:]...)
	return s.write(f)
}

// Clear removes every remembered rule.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
