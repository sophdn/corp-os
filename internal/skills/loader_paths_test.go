package skills

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestDirReturnsOverlayRoot(t *testing.T) {
	if got := New("/some/tree").Dir(); got != "/some/tree" {
		t.Errorf("Dir() = %q, want /some/tree", got)
	}
}

// A stat error that is NOT fs.ErrNotExist must propagate (a non-directory parent
// in the overlay path → ENOTDIR), rather than being swallowed as "absent tree".
func TestDiscoverDiskPropagatesStatError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	writeFile(t, file, "x")
	// stat-ing "<file>/child" returns ENOTDIR, not ErrNotExist.
	if _, err := New(filepath.Join(file, "child")).Discover(); err == nil {
		t.Error("expected a propagated stat error for a non-directory parent, got nil")
	}
}

// errFS fails every Open, so fs.ReadDir returns an error — exercising discoverFS's
// error branch directly (unreachable via the embedded FS, which never errors).
type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, errors.New("boom") }

func TestDiscoverFSReadDirError(t *testing.T) {
	if _, err := discoverFS(errFS{}, "anything"); err == nil {
		t.Error("discoverFS should surface a ReadDir error, got nil")
	}
}
