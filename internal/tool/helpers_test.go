package tool

import "testing"

func TestFail_ShapesAFailedResult(t *testing.T) {
	call := Call{ID: "c1", Surface: "fs", Action: "read"}
	r := Fail(call, ClassTool, "boom", 42)
	if r.OK {
		t.Error("Fail must produce OK=false")
	}
	if r.Call.ID != call.ID || r.Call.Surface != call.Surface || r.Call.Action != call.Action {
		t.Errorf("Call = %+v, want %+v", r.Call, call)
	}
	if r.ErrorClass != ClassTool || r.LatencyMS != 42 {
		t.Errorf("class/latency = %s/%d, want tool_error/42", r.ErrorClass, r.LatencyMS)
	}
	m, ok := r.Value.(map[string]any)
	if !ok || m["error"] != "boom" {
		t.Errorf("Value = %+v, want {\"error\":\"boom\"}", r.Value)
	}
}

type decodeTarget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestDecodeValue(t *testing.T) {
	// A map[string]any (the usual decoded dispatch Value) round-trips into the typed struct.
	got, ok := DecodeValue[decodeTarget](map[string]any{"name": "x", "count": 3})
	if !ok || got.Name != "x" || got.Count != 3 {
		t.Fatalf("DecodeValue ok=%v got=%+v, want {x 3}", ok, got)
	}
	// A value json.Marshal cannot encode (a channel) → ok=false, zero value.
	if _, ok := DecodeValue[decodeTarget](make(chan int)); ok {
		t.Error("DecodeValue should fail on an unmarshalable value")
	}
	// A shape that marshals but does not unmarshal into T (string where int is wanted) → false.
	if _, ok := DecodeValue[decodeTarget](map[string]any{"count": "not-an-int"}); ok {
		t.Error("DecodeValue should fail on a type-mismatched field")
	}
}
