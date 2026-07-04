package jsonx

import (
	"encoding/json"
	"testing"
)

type base struct {
	A string `json:"a"`
}

type withExtra struct {
	base
	C     string                     `json:"c"`
	Skip  string                     `json:"-"`
	Extra map[string]json.RawMessage `json:"-"`
}

func TestUnmarshalCapturesUnknownKeys(t *testing.T) {
	var v withExtra
	if err := Unmarshal([]byte(`{"a":"x","c":"y","d":2,"e":{"nested":true}}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "x" || v.C != "y" {
		t.Errorf("known fields wrong: %+v", v)
	}
	if len(v.Extra) != 2 || string(v.Extra["d"]) != "2" || string(v.Extra["e"]) != `{"nested":true}` {
		t.Errorf("Extra = %v, want d and e captured", v.Extra)
	}
}

func TestUnmarshalNoUnknownKeysLeavesExtraNil(t *testing.T) {
	var v withExtra
	if err := Unmarshal([]byte(`{"a":"x","c":"y"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Extra != nil {
		t.Errorf("Extra should stay nil when nothing is unknown: %v", v.Extra)
	}
}

func TestUnmarshalWithoutExtraField(t *testing.T) {
	var v base
	if err := Unmarshal([]byte(`{"a":"x","zz":1}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "x" {
		t.Errorf("plain decode broken: %+v", v)
	}
}
