package repl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"corpos/internal/agent"
	"corpos/internal/tool"
)

// fakeTurn records the prompts it is asked to run and replies from a script.
// failFirst makes the first call return an error (to exercise the continue path).
type fakeTurn struct {
	prompts     []string
	replies     map[string]agent.Result
	failOn      string
	hadDeadline bool
}

func (f *fakeTurn) Run(ctx context.Context, prompt string) (agent.Result, error) {
	f.prompts = append(f.prompts, prompt)
	if _, ok := ctx.Deadline(); ok {
		f.hadDeadline = true
	}
	if prompt == f.failOn {
		return agent.Result{}, errors.New("boom")
	}
	if r, ok := f.replies[prompt]; ok {
		return r, nil
	}
	return agent.Result{Text: "ans:" + prompt}, nil
}

func runREPL(in string, t *fakeTurn, cfg Config) (out, errOut string, err error) {
	var o, e strings.Builder
	err = Run(context.Background(), strings.NewReader(in), &o, &e, t, cfg)
	return o.String(), e.String(), err
}

func TestRun_MultipleTurnsInOrder(t *testing.T) {
	ft := &fakeTurn{}
	out, _, err := runREPL("hello\nworld\n", ft, Config{Prompt: "> "})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ft.prompts) != 2 || ft.prompts[0] != "hello" || ft.prompts[1] != "world" {
		t.Fatalf("prompts = %v, want [hello world]", ft.prompts)
	}
	if !strings.Contains(out, "ans:hello") || !strings.Contains(out, "ans:world") {
		t.Errorf("out missing answers: %q", out)
	}
	if strings.Index(out, "ans:hello") > strings.Index(out, "ans:world") {
		t.Errorf("answers out of order: %q", out)
	}
}

func TestRun_ExitCommandStops(t *testing.T) {
	for _, word := range []string{"exit", "quit", ":q", "EXIT"} {
		ft := &fakeTurn{}
		if _, _, err := runREPL("hi\n"+word+"\nnever\n", ft, Config{}); err != nil {
			t.Fatalf("%s: Run: %v", word, err)
		}
		if len(ft.prompts) != 1 || ft.prompts[0] != "hi" {
			t.Errorf("%s: prompts = %v, want only [hi]", word, ft.prompts)
		}
	}
}

func TestRun_EOFClosesCleanly(t *testing.T) {
	ft := &fakeTurn{}
	_, _, err := runREPL("only\n", ft, Config{})
	if err != nil {
		t.Fatalf("EOF should close cleanly, got %v", err)
	}
	if len(ft.prompts) != 1 {
		t.Errorf("prompts = %v, want one", ft.prompts)
	}
}

func TestRun_BlankAndWhitespaceLinesSkipped(t *testing.T) {
	ft := &fakeTurn{}
	if _, _, err := runREPL("\n   \nhi\n\n", ft, Config{Prompt: "> "}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ft.prompts) != 1 || ft.prompts[0] != "hi" {
		t.Errorf("prompts = %v, want only [hi]", ft.prompts)
	}
}

func TestRun_TurnErrorIsReportedAndLoopContinues(t *testing.T) {
	ft := &fakeTurn{failOn: "bad"}
	out, errOut, err := runREPL("bad\ngood\n", ft, Config{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ft.prompts) != 2 {
		t.Errorf("loop should continue past a failed turn: prompts = %v", ft.prompts)
	}
	if !strings.Contains(errOut, "boom") {
		t.Errorf("turn error not reported to errOut: %q", errOut)
	}
	if !strings.Contains(out, "ans:good") {
		t.Errorf("turn after error did not run: %q", out)
	}
}

func TestRun_ToolStatusGoesToErrOut(t *testing.T) {
	ft := &fakeTurn{replies: map[string]agent.Result{
		"q": {Text: "done", Dispatches: []tool.Result{
			{Call: tool.Call{Surface: "work", Action: "chain_find"}, OK: true},
			{Call: tool.Call{Surface: "work", Action: "chain_status"}, OK: false, ErrorClass: tool.ClassTool},
		}},
	}}
	out, errOut, _ := runREPL("q\n", ft, Config{})
	if !strings.Contains(errOut, "[tool work.chain_find -> ok]") {
		t.Errorf("ok dispatch missing from errOut: %q", errOut)
	}
	if !strings.Contains(errOut, "[tool work.chain_status -> tool_error]") {
		t.Errorf("failed dispatch missing from errOut: %q", errOut)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("answer missing from out: %q", out)
	}
}

func TestRun_CompactionNoticeGoesToErrOut(t *testing.T) {
	ft := &fakeTurn{replies: map[string]agent.Result{
		"q": {Text: "done", Compaction: &agent.CompactionEvent{TurnIndex: 4, TokensBefore: 9000, TokensAfter: 3000, GroupsEvicted: 3, Budget: 8000}},
	}}
	out, errOut, _ := runREPL("q\n", ft, Config{})
	if !strings.Contains(errOut, "context compacted at turn 4: 9000→3000 tok (budget 8000), 3 turn-group(s) summarized") {
		t.Errorf("compaction notice missing from errOut: %q", errOut)
	}
	if strings.Contains(errOut, "still over budget") {
		t.Errorf("under-budget compaction should not warn: %q", errOut)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("answer missing from out: %q", out)
	}
}

func TestRun_CompactionOverBudgetWarns(t *testing.T) {
	ft := &fakeTurn{replies: map[string]agent.Result{
		"q": {Text: "done", Compaction: &agent.CompactionEvent{TurnIndex: 2, TokensBefore: 6000, TokensAfter: 5200, GroupsEvicted: 1, Budget: 4000, Overhead: 5000}},
	}}
	_, errOut, _ := runREPL("q\n", ft, Config{})
	if !strings.Contains(errOut, "still over budget 4000") || !strings.Contains(errOut, "overhead ~5000") {
		t.Errorf("over-budget saturation note missing: %q", errOut)
	}
}

func TestRun_TurnTimeoutAttachesDeadline(t *testing.T) {
	ft := &fakeTurn{}
	if _, _, err := runREPL("hi\n", ft, Config{TurnTimeout: time.Minute}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ft.hadDeadline {
		t.Errorf("turn context should carry a deadline when TurnTimeout > 0")
	}
}

// errReader returns a non-EOF error so the scanner-error branch is exercised.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read fault") }

func TestRun_ScannerErrorIsWrapped(t *testing.T) {
	var o, e strings.Builder
	err := Run(context.Background(), errReader{}, &o, &e, &fakeTurn{}, Config{})
	if err == nil || !strings.Contains(err.Error(), "read stdin") {
		t.Fatalf("want wrapped read error, got %v", err)
	}
}
