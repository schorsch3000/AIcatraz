// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// projectFile, in a project's state directory, holds the path of its config, so
// that `clean` can tell a project that is gone from one that is merely idle.
const projectFile = "project"

// recordProject notes where the project's config lives. Best effort.
func recordProject(state, cfgAbs string) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return
	}
	path := filepath.Join(state, projectFile)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == cfgAbs {
		return
	}
	os.WriteFile(path, []byte(cfgAbs+"\n"), 0o600)
}

// Status implements `aicatraz status`: the running jails of all projects.
func Status(stdout io.Writer) error {
	out, err := exec.Command("docker", "ps", "--filter", "label=aicatraz.project",
		"--format", `{{.Label "aicatraz.project"}}|{{.Label "aicatraz.egress"}}|{{.Names}}|{{.Image}}|{{.Status}}`).Output()
	if err != nil {
		return dockerError("docker ps", err)
	}
	type row struct{ project, name, image, status string }
	var rows []row
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 || f[1] != "" { // the network helpers are not jails
			continue
		}
		rows = append(rows, row{f[0], f[2], f[3], f[4]})
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no running jails")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].project < rows[j].project })
	st := ui.For(stdout)
	w := 0
	for _, r := range rows {
		w = max(w, len(r.project))
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s  %s  %s\n", st.BoldCyan(fmt.Sprintf("%-*s", w, r.project)), st.Dim(r.status), r.name)
	}
	return nil
}

// dockerError turns a failed docker call into something to act on.
func dockerError(what string, err error) error {
	var ee *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("docker is not installed or not on PATH")
	case errors.As(err, &ee):
		msg := strings.TrimSpace(string(ee.Stderr))
		if strings.Contains(msg, "permission denied") {
			return fmt.Errorf("%s: permission denied on the Docker socket; add yourself to the docker group", what)
		}
		if msg != "" {
			return fmt.Errorf("%s: %s", what, msg)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// LogOptions are the flags of `aicatraz log`.
type LogOptions struct {
	ConfigPath string
	Lines      int // 0: all
	Follow     bool
}

// Log implements `aicatraz log`: the audit log of the project.
func Log(o LogOptions, stdout io.Writer) error {
	_, id, err := loadForID(o.ConfigPath)
	if err != nil {
		return err
	}
	state, err := StateDir(id)
	if err != nil {
		return err
	}
	path := filepath.Join(state, "audit.log")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "nothing logged for this project yet")
		if !o.Follow {
			return nil
		}
		for errors.Is(err, os.ErrNotExist) {
			time.Sleep(500 * time.Millisecond)
			f, err = os.Open(path)
		}
	}
	if err != nil {
		return err
	}
	defer f.Close()

	st := ui.For(stdout)
	var lines []string
	r := bufio.NewReader(f)
	for {
		l, err := r.ReadString('\n')
		if strings.HasSuffix(l, "\n") {
			lines = append(lines, strings.TrimRight(l, "\n"))
		}
		if err != nil {
			break
		}
	}
	if len(lines) == 0 && !o.Follow {
		fmt.Fprintln(stdout, "nothing logged for this project yet")
		return nil
	}
	if o.Lines > 0 && len(lines) > o.Lines {
		lines = lines[len(lines)-o.Lines:]
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, FormatAudit(l, st))
	}
	pending := ""
	for o.Follow {
		l, err := r.ReadString('\n')
		pending += l
		if strings.HasSuffix(pending, "\n") { // a line may arrive in two pieces
			fmt.Fprintln(stdout, FormatAudit(strings.TrimRight(pending, "\n"), st))
			pending = ""
		}
		if err != nil {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return nil
}

// FormatAudit renders one audit log line for people. A line that isn't JSON is
// shown as it is.
func FormatAudit(line string, st ui.Style) string {
	var e broker.Entry
	if json.Unmarshal([]byte(line), &e) != nil {
		return line
	}
	what := strings.Join(append([]string{e.Name}, e.Args...), " ")
	var b strings.Builder
	b.WriteString(st.Dim(e.Time.Local().Format("2006-01-02 15:04:05")))
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%-8s", e.Event))
	if what = strings.TrimSpace(what); what != "" {
		b.WriteString(" " + st.Bold(what))
	}
	if e.Decision != "" {
		d := e.Decision
		switch d {
		case "allow", "allowed":
			d = st.Green(d)
		case "deny", "denied", "block", "blocked":
			d = st.Red(d)
		default:
			d = st.Yellow(d)
		}
		b.WriteString("  " + d)
		if e.DecidedBy != "" {
			b.WriteString(st.Dim(" (" + e.DecidedBy + ")"))
		}
	}
	if e.ExitCode != nil {
		b.WriteString("  exit " + strconv.Itoa(*e.ExitCode))
		if e.DurationMS > 0 {
			b.WriteString(st.Dim(fmt.Sprintf(" in %s", (time.Duration(e.DurationMS) * time.Millisecond).Round(time.Millisecond))))
		}
	}
	if e.Pins == "attention" {
		b.WriteString("  " + st.BoldYellow("pinned files changed"))
	}
	if e.Message != "" {
		b.WriteString("  " + st.Dim(e.Message))
	}
	return b.String()
}

// Doctor implements `aicatraz doctor`: what is in order and what is not. It
// returns false if something that stops AIcatraz from working is wrong.
func Doctor(cfgPath string, stdout io.Writer) bool {
	st := ui.For(stdout)
	ok := true
	check := func(good bool, fatal bool, name, detail string) {
		mark := st.BoldGreen("ok  ")
		switch {
		case !good && fatal:
			mark, ok = st.BoldRed("FAIL"), false
		case !good:
			mark = st.BoldYellow("warn")
		}
		fmt.Fprintf(stdout, "%s  %s", mark, name)
		if detail != "" {
			fmt.Fprintf(stdout, "  %s", st.Dim(detail))
		}
		fmt.Fprintln(stdout)
	}

	if _, err := exec.LookPath("docker"); err != nil {
		check(false, true, "docker", "not found on PATH; install Docker")
	} else {
		out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
		if err != nil {
			check(false, true, "docker daemon", dockerError("docker version", err).Error())
		} else {
			check(true, true, "docker daemon", "version "+strings.TrimSpace(string(out)))
		}
	}
	if p, err := exec.LookPath("tmux"); err != nil {
		check(false, false, "tmux", "not found; only `up --tmux` needs it")
	} else {
		check(true, false, "tmux", p)
	}
	if base, err := RunBase(); err != nil {
		check(false, true, "runtime directory", err.Error())
	} else {
		check(true, true, "runtime directory", base)
	}
	if dir, err := StateDir("x"); err != nil {
		check(false, true, "state directory", err.Error())
	} else {
		dir = filepath.Dir(dir)
		for {
			if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
				break
			}
			dir = filepath.Dir(dir)
		}
		check(syscall.Access(dir, 2) == nil, true, "state directory", "writable under "+dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := config.UserConfigPath(home)
		if _, err := os.Stat(p); err != nil {
			check(false, false, "user config", "none yet ("+p+"); optional")
		} else {
			check(true, false, "user config", p)
		}
	}
	if _, err := os.Stat(cfgPath); err != nil {
		check(false, false, "project config", cfgPath+" not found; `aicatraz init` writes one")
	} else {
		_, problems, err := config.Load(cfgPath, config.Options{})
		switch {
		case err != nil:
			check(false, true, "project config", err.Error())
		case config.HasErrors(problems):
			check(false, true, "project config", cfgPath+" has errors; run `aicatraz config check`")
		default:
			check(true, true, "project config", cfgPath)
		}
	}
	return ok
}

// staleProject is a state directory whose project directory is gone.
type staleProject struct {
	id, state, config string
}

// Clean implements `aicatraz clean`: it removes the state, and the images, of
// projects whose config file is gone. State recorded by an older version has no
// path to check, and is kept. Without yes it lists, asks, and removes only on y.
func Clean(yes bool, in io.Reader, interactive bool, stdout, stderr io.Writer) error {
	probe, err := StateDir("x")
	if err != nil {
		return err
	}
	base := filepath.Dir(probe)
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "nothing to clean")
		return nil
	}
	if err != nil {
		return err
	}
	var stale []staleProject
	unknown := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		state := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(state, projectFile))
		if err != nil {
			unknown++
			continue
		}
		cfg := strings.TrimSpace(string(b))
		if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
			stale = append(stale, staleProject{e.Name(), state, cfg})
		}
	}
	st := ui.For(stdout)
	if unknown > 0 {
		fmt.Fprintf(stdout, "%s %d project(s) have no recorded location (started with an older version) and are kept; they get one the next time they run\n", st.Dim("note:"), unknown)
	}
	if len(stale) == 0 {
		fmt.Fprintln(stdout, "nothing to clean")
		return nil
	}
	for _, s := range stale {
		fmt.Fprintf(stdout, "%s  %s  %s\n", st.BoldCyan(s.id), st.Dim("config gone:"), s.config)
	}
	if !yes {
		if !interactive {
			return fmt.Errorf("not removing without asking; run it in a terminal, or pass --yes")
		}
		fmt.Fprintf(stderr, "Remove the state and images of %d project(s)? [y/N] ", len(stale))
		a, _ := bufio.NewReader(in).ReadString('\n')
		if a = strings.ToLower(strings.TrimSpace(a)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "nothing removed")
			return nil
		}
	}
	for _, s := range stale {
		for _, tag := range []string{"latest", "sudo"} {
			ref := "aicatraz/" + s.id + ":" + tag
			if exec.Command("docker", "image", "inspect", ref).Run() == nil {
				exec.Command("docker", "rmi", ref).Run()
			}
		}
		if err := os.RemoveAll(s.state); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("removed"), s.id)
	}
	return nil
}
