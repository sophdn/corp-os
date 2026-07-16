package fsorgan

import (
	"os"
	"path/filepath"
	"strings"
)

// expandUserPath expands a leading ~ to the user's home directory: a bare "~"
// becomes $HOME and "~/x" becomes $HOME/x. Any other path — absolute, relative,
// or a "~user" form we deliberately do not resolve — is returned unchanged.
//
// This is the single tilde-resolution point for the organ. Without it a leading
// ~ is not special to the OS, so os.Stat resolves "~/x" against corpos's working
// directory and silently creates a literal "~" directory while reporting
// success. Every handler routes its caller-supplied path through this before
// touching the filesystem.
func expandUserPath(path string) string {
	if path == "~" {
		if home := homeDir(); home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home := homeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// absPath returns the absolute form of p, or p unchanged when it cannot be
// resolved. The single absolute-path helper for the organ (read-state keying and
// the remove protected-root check).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// homeDir returns the user's home directory, or "" when it cannot be resolved
// (in which case the caller leaves the path untouched — a "~/x" that cannot be
// expanded is better surfaced as a missing path than written to the wrong place).
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
