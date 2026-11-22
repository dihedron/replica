package vehov1

import (
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/protobuf/proto"
)

// ETag is the entity tag of a job's view (.specs/veho-api-server.md §5): a
// hash of its deterministic encoding, the same for the server and for a
// client that decoded the view, over gRPC or REST.
func ETag(j *Job) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:12]) + `"`
}
