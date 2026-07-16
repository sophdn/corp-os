package version

import "testing"

func TestVersionNotEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() must not be empty")
	}
}

func TestVersionIsDev(t *testing.T) {
	if got := Version(); got != "0.0.0-dev" {
		t.Fatalf("Version() = %q, want %q", got, "0.0.0-dev")
	}
}
