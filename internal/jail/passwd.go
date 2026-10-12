package jail

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// MergePasswd returns the image's passwd file with an entry for the host
// user added, unless the image already has that uid. The home directory in an
// existing entry is left alone; $HOME is set through the environment.
func MergePasswd(existing string, uid, gid int, name, home string) string {
	if hasID(existing, 2, uid) {
		return existing
	}
	name = freeName(existing, name)
	return appendLine(existing, fmt.Sprintf("%s:x:%d:%d:%s:%s:/bin/sh", name, uid, gid, name, home))
}

// MergeGroup is MergePasswd for /etc/group.
func MergeGroup(existing string, gid int, name string) string {
	if hasID(existing, 2, gid) {
		return existing
	}
	name = freeName(existing, name)
	return appendLine(existing, fmt.Sprintf("%s:x:%d:", name, gid))
}

// hasID reports whether any line has id in the given colon-separated field.
func hasID(content string, field, id int) bool {
	want := strconv.Itoa(id)
	for _, line := range strings.Split(content, "\n") {
		parts := strings.Split(line, ":")
		if len(parts) > field && parts[field] == want {
			return true
		}
	}
	return false
}

// freeName returns name, or a fallback if the image already uses it.
func freeName(content, name string) string {
	if name == "" {
		name = "aicatraz"
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.SplitN(line, ":", 2)[0] == name {
			return "aicatraz"
		}
	}
	return name
}

func appendLine(content, line string) string {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + line + "\n"
}

// imageFile reads a file out of an image without running it (so it works for
// images without a shell, such as distroless). A missing file yields "".
func imageFile(docker, image, file string) (string, error) {
	out, err := exec.Command(docker, "create", image).Output()
	if err != nil {
		return "", fmt.Errorf("docker create %s: %w", image, withStderr(err))
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command(docker, "rm", "-f", id).Run()

	cp := exec.Command(docker, "cp", id+":"+file, "-")
	var stdout, stderr bytes.Buffer
	cp.Stdout, cp.Stderr = &stdout, &stderr
	if err := cp.Run(); err != nil {
		if strings.Contains(stderr.String(), "Could not find the file") ||
			strings.Contains(stderr.String(), "No such") {
			return "", nil
		}
		return "", fmt.Errorf("docker cp %s:%s: %s", image, file, strings.TrimSpace(stderr.String()))
	}
	tr := tar.NewReader(&stdout)
	if _, err := tr.Next(); err != nil {
		if err == io.EOF {
			return "", nil
		}
		return "", err
	}
	data, err := io.ReadAll(tr)
	return string(data), err
}

func withStderr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// imagePath returns the PATH an image defines, or "" if it has none.
func imagePath(docker, image string) string {
	out, err := exec.Command(docker, "image", "inspect", "--format", `{{range .Config.Env}}{{println .}}{{end}}`, image).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "PATH="); ok {
			return v
		}
	}
	return ""
}
