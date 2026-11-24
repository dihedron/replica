// Package packaging holds what veho's packages install, and the public keys
// compiled into veho that verify the manifests of its plugins
// (.specs/veho-plugin-distribution.md §3).
package packaging

import (
	"crypto/ed25519"

	_ "embed"

	"github.com/dihedron/veho/pkg/plugin/process"
)

//go:embed plugin-signing.pub
var pluginSigningKeys []byte

// PluginKeys are the keys a plugin's manifest may be signed with.
func PluginKeys() ([]ed25519.PublicKey, error) { return process.ParseKeys(pluginSigningKeys) }
