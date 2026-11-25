package plugin

import (
	"encoding/json"
	"testing"
)

// TestDecodeSettings: empty settings are {}, known keys decode, unknown
// keys are refused (veho-plugin-distribution.md §4).
func TestDecodeSettings(t *testing.T) {
	var v struct {
		Roots []string `json:"roots"`
	}
	for _, empty := range []string{"", "  ", "{}"} {
		if err := DecodeSettings(json.RawMessage(empty), &v); err != nil {
			t.Errorf("%q: %v", empty, err)
		}
	}
	if err := DecodeSettings(json.RawMessage(`{"roots":["/a"]}`), &v); err != nil || len(v.Roots) != 1 || v.Roots[0] != "/a" {
		t.Errorf("roots: %v %v", v.Roots, err)
	}
	if err := DecodeSettings(json.RawMessage(`{"rots":["/a"]}`), &v); err == nil {
		t.Error("an unknown key was accepted")
	}
	if err := DecodeSettings(json.RawMessage(`[`), &v); err == nil {
		t.Error("malformed settings were accepted")
	}
}
