// This is what corpos actually produced — the ONLY file it changed. The logic is
// correct and `go test ./calc/` passes, but the implementation lives INSIDE this
// *_test.go file, so it is test-only build scope: a real importer of the package
// gets nothing. That is the "hollow green / test-only diff" the gate rejects,
// which is why corpos did not declare done. See ./README.md.
package calc

import (
	"errors"
	"testing"
)

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Sub returns the difference of a and b.
func Sub(a, b int) int {
	return a - b
}

// Mul returns the product of a and b.
func Mul(a, b int) int {
	return a * b
}

// Div returns the quotient of a and b.
// It returns an error if b is 0.
func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, errors.New("cannot divide by zero")
	}
	return a / b, nil
}

func TestCalc(t *testing.T) {
	tests := []struct {
		name      string
		a, b      int
		want      int
		wantErr   bool
		operation func(int, int) (int, error)
	}{
		{"Add", 2, 3, 5, false, func(a, b int) (int, error) { return Add(a, b), nil }},
		{"Sub", 5, 3, 2, false, func(a, b int) (int, error) { return Sub(a, b), nil }},
		{"Mul", 2, 3, 6, false, func(a, b int) (int, error) { return Mul(a, b), nil }},
		{"Div", 6, 3, 2, false, func(a, b int) (int, error) { return Div(a, b) }},
		{"DivZero", 6, 0, 0, true, func(a, b int) (int, error) { return Div(a, b) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.operation(tt.a, tt.b)
			if (err != nil) != tt.wantErr {
				t.Errorf("%s(%d, %d) error = %v, wantErr %v", tt.name, tt.a, tt.b, err, tt.wantErr)
			}
			if got != tt.want && !tt.wantErr {
				t.Errorf("%s(%d, %d) = %d, want %d", tt.name, tt.a, tt.b, got, tt.want)
			}
		})
	}
}
