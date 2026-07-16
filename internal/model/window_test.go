package model

import "testing"

func TestKnownContextWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want int
	}{
		{"google/gemini-3.1-flash-lite", 1_000_000}, // mid rung
		{"gemini-2.0-pro", 1_000_000},
		{"deepseek/deepseek-v3.2", 163_840}, // coding rung
		{"claude-opus-4-8", 200_000},        // strong rung (standard window)
		{"claude-sonnet-4-6", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"claude-opus-4-8[1m]", 1_000_000},      // explicit 1M variant
		{"Qwen2.5-32B-Instruct-Q4_K_M.gguf", 0}, // local: unknown to the table (probe owns it)
		{"some-unrecognized-model", 0},
	}
	for _, c := range cases {
		if got := KnownContextWindow(c.id); got != c.want {
			t.Errorf("KnownContextWindow(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}
