package packaging

import "testing"

// TestPluginKeys: the compiled-in plugin-signing keys parse, and there is
// one, or no release plugin could start.
func TestPluginKeys(t *testing.T) {
	keys, err := PluginKeys()
	if err != nil || len(keys) == 0 {
		t.Fatalf("%d keys, %v", len(keys), err)
	}
}
