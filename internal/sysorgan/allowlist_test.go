package sysorgan

import (
	"strings"
	"testing"

	"corpos/internal/shellsafe"
)

// TestAllowlist_ShellShapeCorpus pins the sys.exec allowlist to the shared shell-shape decision
// table (bug 1107): permit must reject exactly the dangerous-shape commands and accept the
// benign ones (all of which use allowlisted heads), so it cannot drift from the build-test risk
// gate, which asserts the same corpus.
func TestAllowlist_ShellShapeCorpus(t *testing.T) {
	a := loadAllowlist("")
	for _, c := range shellsafe.Corpus {
		err := a.permit(c.Command)
		if (err != nil) != c.Reject {
			t.Errorf("permit(%q) err=%v, want reject=%v — %s", c.Command, err, c.Reject, c.Note)
		}
	}
}

func TestAllowlist_Permit(t *testing.T) {
	a := loadAllowlist("")
	cases := []struct {
		command string
		ok      bool
		reason  string
	}{
		{"git status", true, ""},
		{"go test ./...", true, ""},
		{"gofmt -s -l .", true, ""},                 // the gate's formatting authority — a coding worker must self-check it
		{"echo hi | grep h", true, ""},              // compound, all allowed
		{"git log; make build", true, ""},           // segmented, all allowed
		{"FOO=bar go test", true, ""},               // leading env assignment skipped
		{"", false, "empty"},                        // empty
		{"   ", false, "empty"},                     // whitespace
		{"bash -c 'rm -rf /'", false, "allowlist"},  // shell excluded
		{"sh script.sh", false, "allowlist"},        // shell excluded
		{"git status; rm file", false, "allowlist"}, // one disallowed segment
		{"echo $(whoami)", false, "command substitution"},
		{"echo `id`", false, "command substitution"},
		{"FOO=bar", false, "no command"}, // only an assignment, no head
	}
	for _, c := range cases {
		err := a.permit(c.command)
		if c.ok {
			if err != nil {
				t.Errorf("permit(%q) = %v, want ok", c.command, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("permit(%q) = ok, want error containing %q", c.command, c.reason)
			continue
		}
		if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("permit(%q) error = %q, want it to contain %q", c.command, err, c.reason)
		}
	}
}

func TestAllowlist_EnvExtension(t *testing.T) {
	a := loadAllowlist("kubectl, terraform ,")
	if err := a.permit("kubectl get pods"); err != nil {
		t.Fatalf("extended command should be permitted: %v", err)
	}
	if err := a.permit("terraform plan"); err != nil {
		t.Fatalf("extended command should be permitted: %v", err)
	}
	if err := a.permit("helm install"); err == nil {
		t.Fatal("a command outside the extended allowlist must be rejected")
	}
}

func TestLoadAllowlistFromEnv(t *testing.T) {
	t.Setenv("CORPOS_EXEC_ALLOWLIST", "kubectl")
	a := loadAllowlistFromEnv()
	if err := a.permit("kubectl version"); err != nil {
		t.Fatalf("env-extended command should be permitted: %v", err)
	}
}

func TestIsEnvAssignment(t *testing.T) {
	cases := map[string]bool{
		"FOO=bar":   true,
		"A_B=1":     true,
		"x=y":       true,
		"=bar":      false, // no name
		"FOO":       false, // no =
		"a-b=1":     false, // invalid char in name
		"1=2":       true,  // digits allowed in name body
		"./cmd=arg": false,
	}
	for in, want := range cases {
		if got := isEnvAssignment(in); got != want {
			t.Errorf("isEnvAssignment(%q) = %v, want %v", in, got, want)
		}
	}
}
