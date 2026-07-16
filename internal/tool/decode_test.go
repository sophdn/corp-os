package tool

import "testing"

func TestCommandString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passes through", "go test ./...", "go test ./..."},
		{"list of any salvaged", []any{"go", "test", "./..."}, "go test ./..."},
		{"list of string salvaged", []string{"gofmt", "-l", "."}, "gofmt -l ."},
		{"empty tokens dropped", []any{"go", "", "build"}, "go build"},
		{"non-string element not salvageable", []any{"go", 7}, ""},
		{"number is not a command", 42, ""},
		{"nil is not a command", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandString(tc.in); got != tc.want {
				t.Errorf("CommandString(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
