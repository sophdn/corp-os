package fsorgan

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAbsPath ensures that absolute paths remain unchanged and that relative paths resolve to absolute paths.
func TestAbsPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"absolute path", "/path/to/file", "/path/to/file"},
		{"relative path", "path/to/file", filepath.Join(os.Getenv("PWD"), "path/to/file")},
		{"current directory", "./file", filepath.Join(os.Getenv("PWD"), "file")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := absPath(tt.input)
			if actual != tt.expected {
				t.Errorf("absPath(%q) = %q, want %q", tt.input, actual, tt.expected)
			}
			if !filepath.IsAbs(actual) {
				t.Errorf("absPath(%q) is not an absolute path: %q", tt.input, actual)
			}
		})
	}
}

// TestExpandUserPathUnique ensures that '~' expands to homeDir(), '~/sub' expands properly, and non-tilde paths return unchanged.
func TestExpandUserPathUnique(t *testing.T) {
	home := homeDir()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"tilde", "~", home},
		{"tilde with subpath", "~/sub", filepath.Join(home, "sub")},
		{"non-tilde path", "/path/to/file", "/path/to/file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := expandUserPath(tt.input)
			if actual != tt.expected {
				t.Errorf("expandUserPath(%q) = %q, want %q", tt.input, actual, tt.expected)
			}
		})
	}
}

// TestHomeDir ensures that homeDir() returns the expected home directory.
func TestHomeDir(t *testing.T) {
	expected := os.Getenv("HOME")
	actual := homeDir()
	if actual != expected {
		t.Errorf("homeDir() = %q, want %q", actual, expected)
	}
}
