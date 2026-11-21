package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/dihedron/veho/pkg/plugin"
)

// part is one part of an object: [Offset, Offset+Length).
type part struct {
	No     int
	Offset int64
	Length int64
}

// partSize chooses the part size of a job (§4.4): base, raised so that
// the object fits in the destinations' MaxParts, within every destination's
// MinPartSize and MaxPartSize.
func partSize(size, base int64, sinks []plugin.Capabilities) (int64, error) {
	ps, lo, hi := base, int64(1), int64(1<<62)
	for _, c := range sinks {
		if c.MaxParts > 0 {
			ps = max(ps, (size+int64(c.MaxParts)-1)/int64(c.MaxParts))
		}
		lo = max(lo, c.MinPartSize)
		if c.MaxPartSize > 0 {
			hi = min(hi, c.MaxPartSize)
		}
	}
	ps = max(ps, lo)
	if ps > hi {
		return 0, plugin.Wrapf(plugin.Permanent, "engine: no part size fits an object of %d bytes and every destination (%d to %d bytes)", size, ps, hi)
	}
	return ps, nil
}

// plan splits an object into parts of ps bytes, the last shorter; an empty
// object is one empty part, so that every upload has a part to commit.
func plan(size, ps int64) []part {
	if size == 0 {
		return []part{{No: 0}}
	}
	n := int((size + ps - 1) / ps)
	parts := make([]part, n)
	for i := range parts {
		off := int64(i) * ps
		parts[i] = part{No: i, Offset: off, Length: min(ps, size-off)}
	}
	return parts
}

// digest is the object's digest (§4.4): SHA-256 over the part size, the
// size and the parts' SHA-256s in order.
func digest(ps, size int64, parts [][]byte) (string, error) {
	h := sha256.New()
	h.Write([]byte("veho-digest/v1"))
	h.Write(binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, uint64(ps)), uint64(size))) //nolint:gosec // sizes are not negative
	for i, p := range parts {
		if len(p) != sha256.Size {
			return "", fmt.Errorf("engine: part %d has no digest", i)
		}
		h.Write(p)
	}
	if len(parts) != len(plan(size, ps)) {
		return "", errors.New("engine: the parts do not cover the object")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
