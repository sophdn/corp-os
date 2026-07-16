package agent

import (
	"testing"
)

func TestIsLangTag(t *testing.T) {
	tests := []struct {
		input  string
		output bool
	}{
		{"", false},
		{"a123456789012", false},
		{"a12345678901", true},
		{"a123456789012 3", false},
		{"a123456789012\t3", false},
		{"a123456789012{3", false},
		{"a123456789012\"3", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isLangTag(tt.input); got != tt.output {
				t.Errorf("isLangTag(%q) = %v, want %v", tt.input, got, tt.output)
			}
		})
	}
}

func TestBalancedObjects(t *testing.T) {
	tests := []struct {
		input  string
		output []string
	}{
		{"{\"a\":\"}\"}", []string{"{\"a\":\"}\"}"}},
		{"{\"a\":\"\\\"\"}", []string{"{\"a\":\"\\\"\"}"}},
		{"{\"b\":{\"c\":1}}", []string{"{\"b\":{\"c\":1}}"}},
		{"{\"a\":1},{\"b\":2}", []string{"{\"a\":1}", "{\"b\":2}"}},
		{"a1234567890123", []string{}},
		{"{{", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := allBalancedObjects(tt.input); !equalSlices(got, tt.output) {
				t.Errorf("allBalancedObjects(%q) = %v, want %v", tt.input, got, tt.output)
			}
		})
	}
}

func TestFirstBalancedObject(t *testing.T) {
	input := "{\"a\":1},{\"b\":2}"
	want := "{\"a\":1}"
	if got := firstBalancedObject(input); got != want {
		t.Errorf("firstBalancedObject(%q) = %q, want %q", input, got, want)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
