package sysorgan

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

func goDocDispatch(p *Provider, symbol string) tool.Result {
	return p.Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "go_doc", Params: map[string]any{"symbol": symbol},
	})
}

// TestGoDoc_ResolvesStdlibSignature is the bug 1090 capability: the coding worker can
// resolve a Go symbol's REAL signature via sys.go_doc instead of guessing it. A stdlib
// lookup returns the authoritative func signature.
func TestGoDoc_ResolvesStdlibSignature(t *testing.T) {
	r := goDocDispatch(New(nil), "strings.HasPrefix")
	if !r.OK {
		t.Fatalf("go_doc strings.HasPrefix failed: %v", r.Value)
	}
	out := mustMap(t, r)["output"].(string)
	if !strings.Contains(out, "func HasPrefix") {
		t.Fatalf("go_doc did not return the real signature, got: %q", out)
	}
}

// TestGoDoc_ResolvesInternalSymbol proves the bug's exact scenario: an INTERNAL corpos
// API (the kind the worker hallucinated) resolves to its real declaration. Run from the
// sysorgan package dir, the corpos module is in scope.
func TestGoDoc_ResolvesInternalSymbol(t *testing.T) {
	r := goDocDispatch(New(nil), "corpos/internal/tool.Spec")
	if !r.OK {
		t.Fatalf("go_doc on an internal symbol failed: %v", r.Value)
	}
	out := mustMap(t, r)["output"].(string)
	if !strings.Contains(out, "Spec") {
		t.Fatalf("go_doc did not resolve the internal type, got: %q", out)
	}
}

// TestGoDoc_RejectsInjectionAndFlags is the bounded-lookup contract: only a symbol-shaped
// argument is accepted — shell injection, command substitution, and go-doc flags are
// refused as worker-recoverable usage errors WITHOUT running anything.
func TestGoDoc_RejectsInjectionAndFlags(t *testing.T) {
	bad := []string{
		"strings.HasPrefix; rm -rf /", // command chaining
		"strings.HasPrefix && id",     // operator
		"$(whoami)",                   // command substitution
		"-all ./...",                  // a flag, not a symbol
		"strings.HasPrefix -all",      // trailing flag
		"a b c",                       // too many tokens
		"x|y",                         // pipe metacharacter
	}
	for _, sym := range bad {
		r := goDocDispatch(New(nil), sym)
		if r.OK {
			t.Errorf("go_doc(%q) should be refused, but ran", sym)
		}
		if r.ErrorClass != tool.ClassUsage {
			t.Errorf("go_doc(%q) rejection should be a worker-recoverable usage error, got class %v", sym, r.ErrorClass)
		}
	}
}

// TestGoDoc_RequiresSymbol: an empty/missing symbol is a usage error, not a panic.
func TestGoDoc_RequiresSymbol(t *testing.T) {
	r := goDocDispatch(New(nil), "")
	if r.OK || r.ErrorClass != tool.ClassUsage {
		t.Fatalf("empty symbol should be a usage error, got OK=%v class=%v", r.OK, r.ErrorClass)
	}
}

func TestValidateGoDocSymbol(t *testing.T) {
	ok := []string{"strings.HasPrefix", "corpos/internal/tool.Spec", "bytes.Buffer.Write", "fmt", "encoding/json Marshal", "gopkg.in/yaml.v2"}
	for _, s := range ok {
		if err := validateGoDocSymbol(s); err != nil {
			t.Errorf("validateGoDocSymbol(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"-all", "a;b", "a`b`", "a$b", "a b c", "a|b"}
	for _, s := range bad {
		if err := validateGoDocSymbol(s); err == nil {
			t.Errorf("validateGoDocSymbol(%q) = nil, want an error", s)
		}
	}
}
