package shellsafe

import "testing"

// TestRejectReason_Corpus asserts RejectReason agrees with the shared decision table: it
// returns a reason exactly for the cases the deny rule must reject on shape grounds.
func TestRejectReason_Corpus(t *testing.T) {
	for _, c := range Corpus {
		got := RejectReason(c.Command)
		if (got != "") != c.Reject {
			t.Errorf("RejectReason(%q) = %q (reject=%v), want reject=%v — %s", c.Command, got, got != "", c.Reject, c.Note)
		}
	}
}

// TestRejectReason_NamesTheConstruct: each rejection reason names the construct it caught, so a
// caller's framed message is actionable.
func TestRejectReason_NamesTheConstruct(t *testing.T) {
	cases := map[string]string{
		"echo $(id)":      "command substitution",
		"diff <(a) <(b)":  "process substitution",
		"echo ${X}":       "parameter expansion",
		"go env > /etc/x": "file redirection",
	}
	for cmd, want := range cases {
		if got := RejectReason(cmd); got == "" || !contains(got, want) {
			t.Errorf("RejectReason(%q) = %q, want it to name %q", cmd, got, want)
		}
	}
}

// TestHasFileRedirection_FdDupVsFile pins the fd-dup-vs-file distinction directly.
func TestHasFileRedirection_FdDupVsFile(t *testing.T) {
	benign := []string{"go test 2>&1", "cmd >&2", "cmd <&0", "no redirection here"}
	for _, c := range benign {
		if hasFileRedirection(c) {
			t.Errorf("hasFileRedirection(%q) = true, want false (fd-dup / none)", c)
		}
	}
	files := []string{"a > b", "a >> b", "a < b", "a 2> err.log", "a &> both"}
	for _, c := range files {
		if !hasFileRedirection(c) {
			t.Errorf("hasFileRedirection(%q) = false, want true (file redirect)", c)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
