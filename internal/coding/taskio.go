package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// resolveInputs resolves an AT's typed inputs from upstream ATs' extracted outputs.
// It is branch-aware: it picks the MOST RECENT successful AT whose slug OR
// parent_at_slug matches the reference, so a branch_fix supersedes the original
// target's outputs for downstream consumers.
func (o *Orchestrator) resolveInputs(state *RunState, spec AtomicTask) (map[string]any, error) {
	if len(spec.Inputs) == 0 {
		return nil, nil
	}
	resolved := make(map[string]any, len(spec.Inputs))
	for name, ref := range spec.Inputs {
		var upstream *ATRecord
		for i := len(state.ATs) - 1; i >= 0; i-- {
			cand := &state.ATs[i]
			if cand.Status != ATSuccess {
				continue
			}
			if cand.Slug == ref.From || cand.ParentATSlug == ref.From {
				upstream = cand
				break
			}
		}
		if upstream == nil {
			return nil, fmt.Errorf("input %q references %q but no successful AT with that slug (or parent) was found", name, ref.From)
		}
		val, ok := upstream.Outputs[ref.Field]
		if !ok {
			return nil, fmt.Errorf("input %q references %s.%s but that output is missing from %q; available: %s",
				name, ref.From, ref.Field, upstream.Slug, availableOutputs(upstream.Outputs))
		}
		resolved[name] = val
	}
	return resolved, nil
}

// verifyContract runs an AT's output_contract against the post-success workspace:
// every assertion must exit 0, then each extraction's stdout becomes a named typed
// output. A failed assertion or an unparseable JSON extraction is an error.
func (o *Orchestrator) verifyContract(ctx context.Context, spec AtomicTask, dir string) (map[string]any, error) {
	for _, a := range spec.OutputContract.Assertions {
		run := o.runner.Run(ctx, a.Command, dir, o.gateTimeout)
		if run.ExitCode != 0 {
			return nil, fmt.Errorf("assertion %q failed: %q exited %d\nstderr:\n%s",
				a.Name, strings.Join(a.Command, " "), run.ExitCode, tail(run.Stderr, GateTailBytes))
		}
	}
	if len(spec.OutputContract.Extractions) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(spec.OutputContract.Extractions))
	for _, e := range spec.OutputContract.Extractions {
		run := o.runner.Run(ctx, e.Command, dir, o.gateTimeout)
		if run.ExitCode != 0 {
			return nil, fmt.Errorf("extraction %q command failed: %q exited %d\nstderr:\n%s",
				e.Name, strings.Join(e.Command, " "), run.ExitCode, tail(run.Stderr, GateTailBytes))
		}
		if e.extractionFormat() == FormatJSON {
			var parsed any
			if err := json.Unmarshal([]byte(run.Stdout), &parsed); err != nil {
				return nil, fmt.Errorf("extraction %q declared format=json but stdout is not valid JSON: %w", e.Name, err)
			}
			out[e.Name] = parsed
		} else {
			out[e.Name] = strings.TrimSpace(run.Stdout)
		}
	}
	return out, nil
}

// availableOutputs renders the keys of an outputs map for an error message.
func availableOutputs(m map[string]any) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
