// Package repl is the minimal multi-turn read-eval-print loop for corpos: read a
// prompt from stdin, run one (multi-tool) agent turn, print the answer, repeat
// until EOF or an exit command. It is a thin wrapper over a single agent turn
// runner — the loop's own transcript carries conversation context across turns,
// so the REPL holds no conversation state of its own. Loop-first, polish-last:
// no streaming, no slash commands, no rich TUI (those are a later phase).
package repl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"corpos/internal/agent"
)

// maxLineBytes bounds a single REPL input line so a pathological paste can't
// grow the scan buffer without limit.
const maxLineBytes = 1 << 20 // 1 MiB

// Turn runs one user prompt to a final answer, carrying its own conversation
// context across successive calls. *agent.Loop satisfies it; tests inject a fake
// so the REPL runs with no live model or toolkit-server.
type Turn interface {
	Run(ctx context.Context, prompt string) (agent.Result, error)
}

// Config tunes the REPL surface.
type Config struct {
	// Prompt is the input label written before each read (e.g. "corpos> ").
	Prompt string
	// TurnTimeout bounds each turn; zero means no per-turn deadline.
	TurnTimeout time.Duration
}

// exitWords end the loop when entered as a whole line.
var exitWords = map[string]bool{"exit": true, "quit": true, ":q": true}

// Run drives the REPL until in reaches EOF or the user enters an exit word.
// Answers are written to out; the input label and per-tool status lines go to
// errOut, keeping out a clean stream of just the turn answers (so it pipes).
// ctx is the base context for the whole session; each turn derives a
// TurnTimeout-bounded child from it. A turn error is reported and the loop
// continues — one bad turn does not end the session.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer, t Turn, cfg Config) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	fmt.Fprint(errOut, cfg.Prompt)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Fprint(errOut, cfg.Prompt)
			continue
		}
		if exitWords[strings.ToLower(line)] {
			break
		}

		runTurn(ctx, out, errOut, t, cfg, line)
		fmt.Fprint(errOut, cfg.Prompt)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// runTurn executes one prompt and renders its result. Factored out so the
// per-turn context cancel is scoped tightly via defer.
func runTurn(ctx context.Context, out, errOut io.Writer, t Turn, cfg Config, prompt string) {
	turnCtx := ctx
	if cfg.TurnTimeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, cfg.TurnTimeout)
		defer cancel()
	}

	res, err := t.Run(turnCtx, prompt)
	if err != nil {
		fmt.Fprintf(errOut, "corpos: %v\n", err)
		return
	}
	for _, d := range res.Dispatches {
		status := "ok"
		if !d.OK {
			status = string(d.ErrorClass)
		}
		fmt.Fprintf(errOut, "  [tool %s.%s -> %s]\n", d.Call.Surface, d.Call.Action, status)
	}
	if res.PersistErr != nil {
		fmt.Fprintf(errOut, "corpos: session persist warning: %v\n", res.PersistErr)
	}
	if c := res.Compaction; c != nil {
		fmt.Fprintf(errOut, "corpos: context compacted at turn %d: %d→%d tok (budget %d), %d turn-group(s) summarized\n",
			c.TurnIndex, c.TokensBefore, c.TokensAfter, c.Budget, c.GroupsEvicted)
		if c.OverBudget() {
			fmt.Fprintf(errOut, "corpos: note: still over budget %d — fixed tool-spec overhead ~%d tok dominates; raise -max-context-tokens or narrow the tool surface (-profile)\n",
				c.Budget, c.Overhead)
		}
	}
	fmt.Fprintln(out, res.Text)
}
