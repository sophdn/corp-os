package calc

import "testing"

// The oracle. corpos must make this pass WITHOUT editing this file
// (acceptance-test paths are protected from the worker).
func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", got)
	}
}
