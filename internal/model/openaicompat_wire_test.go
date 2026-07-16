package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"corpos/internal/tool"
)

func TestEncodeArgs(t *testing.T) {
	type args struct {
		call tool.Call
	}
	tests := []struct {
		name  string
		args  args
		want  string
		check func(map[string]any, *testing.T)
	}{
		{
			name: "full call",
			args: args{
				call: tool.Call{
					Action:    "action1",
					Params:    map[string]any{"param1": "value1"},
					Rationale: "rationale1",
				},
			},
			want: `{"action":"action1","params":{"param1":"value1"},"rationale":"rationale1"}`,
			check: func(got map[string]any, t *testing.T) {
				if got["action"] != "action1" || got["params"].(map[string]any)["param1"] != "value1" || got["rationale"] != "rationale1" {
					t.Errorf("expected fields not found in JSON unmarshalled map: %+v", got)
				}
			},
		},
		{
			name: "empty call",
			args: args{
				call: tool.Call{},
			},
			want:  `{"action":""}`,
			check: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeArgs(tt.args.call)
			if got != tt.want {
				t.Errorf("encodeArgs() = %v, want %v", got, tt.want)
			}
			if tt.check != nil {
				var unmarshalled map[string]any
				if err := json.Unmarshal([]byte(got), &unmarshalled); err != nil {
					t.Errorf("failed to unmarshal JSON: %v", err)
				}
				tt.check(unmarshalled, t)
			}
		})
	}
}

func TestToOACTools(t *testing.T) {
	type args struct {
		tools []tool.Spec
	}
	tests := []struct {
		name string
		args args
		want []oacToolDef
	}{
		{
			name: "nil or empty slice",
			args: args{
				tools: nil,
			},
			want: nil,
		},
		{
			name: "populated slice",
			args: args{
				tools: []tool.Spec{
					{
						Name:        "tool1",
						Description: "description1",
						InputSchema: map[string]any{"type": "object"},
					},
				},
			},
			want: []oacToolDef{
				{
					Type: "function",
					Function: oacToolFunction{
						Name:        "tool1",
						Description: "description1",
						Parameters:  map[string]any{"type": "object"},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toOACTools(tt.args.tools)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("toOACTools() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
