package daemon

import (
	"fmt"
	"os"
	"strings"
)

// ParseAllowList reads an allow-exec list out of text: one command per
// line, blank lines and # comments skipped, "*" allowed as it is on the
// command line. A command here is the basename the daemon matches
// against, exactly like -allow-exec's comma-separated form.
func ParseAllowList(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// LoadAllowList reads the file -allow-exec-file names. A file that
// cannot be read is a startup error, not an empty list: silently
// refusing every app is a misconfiguration the operator must see.
func LoadAllowList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("daemon: reading -allow-exec-file %s: %w", path, err)
	}
	return ParseAllowList(data), nil
}
