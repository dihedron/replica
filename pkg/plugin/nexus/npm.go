package nexus

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // npm's shasum
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/stream"
)

// Bounds of what the npm format reads (NX-7, NX-11).
const (
	maxPackument   = 32 << 20
	maxPackageJSON = 1 << 20
)

var (
	npmName   = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]*$`)
	npmSemver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
)

// npmKey is name@version: the name with its scope, the scope without its
// '@', the name without its scope.
type npmKey struct {
	name, scope, base, version string
}

func parseNPM(key string) (npmKey, error) {
	bad := plugin.Wrapf(plugin.Permanent, "nexus: %q is not package@version or @scope/package@version", key)
	at := strings.LastIndex(key, "@")
	if at <= 0 {
		return npmKey{}, bad
	}
	k := npmKey{name: key[:at], version: key[at+1:]}
	k.base = k.name
	if rest, scoped := strings.CutPrefix(k.name, "@"); scoped {
		var ok bool
		if k.scope, k.base, ok = strings.Cut(rest, "/"); !ok || !npmName.MatchString(k.scope) {
			return npmKey{}, bad
		}
	}
	if len(k.name) > 214 || !npmName.MatchString(k.base) || !npmSemver.MatchString(k.version) {
		return npmKey{}, bad
	}
	return k, nil
}

// packument is the package's document URL: a scoped name as @scope%2fname.
func (c *client) packument(k npmKey) string {
	return c.root().String() + url.PathEscape(k.name)
}

type npmDist struct {
	Tarball   string `json:"tarball"`
	Shasum    string `json:"shasum"`
	Integrity string `json:"integrity"`
}

// npm: the key is package@version.
type npm struct{}

// resolve reads the packument and takes the version's tarball (NX-6) and
// its digest: integrity's SHA-512, else the shasum.
func (npm) resolve(ctx context.Context, c *client, key string) (asset, error) {
	k, err := parseNPM(key)
	if err != nil {
		return asset{}, err
	}
	index := c.packument(k)
	b, err := c.get(ctx, index, "the packument of "+k.name, maxPackument)
	if err != nil {
		return asset{}, err
	}
	var doc struct {
		Versions map[string]struct {
			Dist npmDist `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return asset{}, plugin.Wrapf(plugin.Permanent, "nexus: the packument of %s: %v", k.name, err)
	}
	v, ok := doc.Versions[k.version]
	if !ok {
		return asset{}, fmt.Errorf("nexus: %s: %w", key, plugin.ErrNotFound)
	}
	u, err := c.inside(index, v.Dist.Tarball)
	if err != nil {
		return asset{}, err
	}
	a := asset{url: u}
	if s, ok := strings.CutPrefix(v.Dist.Integrity, "sha512-"); ok {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return asset{}, plugin.Wrapf(plugin.Permanent, "nexus: %s: integrity %q", key, v.Dist.Integrity)
		}
		a.algo, a.sum = "sha512", hex.EncodeToString(raw)
	} else if v.Dist.Shasum != "" {
		a.algo, a.sum = "sha1", strings.ToLower(v.Dist.Shasum)
	}
	return a, nil
}

// sink holds the tarball (max_npm_size, NX-10), then publishes it as npm
// does: one PUT of the publish document.
func (npm) sink(_ context.Context, c *client, key string, size int64) (*upload, error) {
	k, err := parseNPM(key)
	if err != nil {
		return nil, err
	}
	if size > c.maxNPMSize {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %s: %d bytes, beyond max_npm_size (%d)", key, size, c.maxNPMSize)
	}
	var sha1sum, sha512sum string
	u := &upload{key: key, limit: c.maxNPMSize}
	u.s = stream.New("nexus: upload of "+key, size, nil, func(ctx context.Context, body io.Reader, _ int64) error {
		if body == nil {
			body = http.NoBody
		}
		data, err := io.ReadAll(io.LimitReader(body, c.maxNPMSize+1))
		switch {
		case err != nil:
			return err
		case int64(len(data)) > c.maxNPMSize:
			return plugin.Wrapf(plugin.Permanent, "nexus: %s: beyond max_npm_size (%d bytes)", key, c.maxNPMSize)
		}
		s1, s512 := sha1.Sum(data), sha512.Sum512(data) //nolint:gosec // npm's shasum
		sha1sum, sha512sum = hex.EncodeToString(s1[:]), hex.EncodeToString(s512[:])
		doc, err := c.publishDocument(k, data, s1[:], s512[:])
		if err != nil {
			return err
		}
		resp, err := c.do(ctx, http.MethodPut, c.packument(k), http.Header{"Content-Type": {"application/json"}}, bytes.NewReader(doc), int64(len(doc)))
		if err != nil {
			return err
		}
		defer discard(resp)
		return uploaded(resp, key, "the version may be there already, and the repository not allow redeploying")
	})
	u.finish = func(ctx context.Context) (plugin.ObjectInfo, error) {
		a, err := npm{}.resolve(ctx, c, key)
		if err != nil {
			return plugin.ObjectInfo{}, err
		}
		if want := map[string]string{"sha512": sha512sum, "sha1": sha1sum}[a.algo]; a.algo != "" && a.sum != want {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Integrity, "nexus: %s: the packument's %s is not that of the tarball sent", key, a.algo)
		}
		return c.checkSHA1(ctx, a.url, key, sha1sum)
	}
	return u, nil
}

// publishDocument is what npm publish sends: the version's package.json,
// read from the tarball and checked against the key (NX-7), its dist,
// the dist-tag, the tarball in base64.
func (c *client) publishDocument(k npmKey, data, s1, s512 []byte) ([]byte, error) {
	manifest, err := packageJSON(data, 4*c.maxNPMSize)
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %s@%s: %v", k.name, k.version, err)
	}
	if manifest["name"] != k.name || manifest["version"] != k.version {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %s@%s: the tarball's package.json is %v@%v", k.name, k.version, manifest["name"], manifest["version"])
	}
	file := k.base + "-" + k.version + ".tgz"
	attachment := file
	if k.scope != "" {
		attachment = "@" + k.scope + "/" + file
	}
	manifest["_id"] = k.name + "@" + k.version
	manifest["dist"] = npmDist{
		Tarball:   c.root().String() + url.PathEscape(k.name) + "/-/" + file,
		Shasum:    hex.EncodeToString(s1),
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(s512),
	}
	return json.Marshal(map[string]any{
		"_id": k.name, "name": k.name,
		"dist-tags": map[string]string{c.distTag: k.version},
		"versions":  map[string]any{k.version: manifest},
		"_attachments": map[string]any{attachment: map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "length": len(data),
		}},
	})
}

// packageJSON is package/package.json of a tarball, reading at most limit
// bytes of the gzip stream and maxPackageJSON of the file.
func packageJSON(data []byte, limit int64) (map[string]any, error) {
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not a gzip tarball: %w", err)
	}
	tr := tar.NewReader(io.LimitReader(z, limit))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no package/package.json (or beyond the bound of what is read)")
		}
		if err != nil {
			return nil, fmt.Errorf("the tarball: %w", err)
		}
		if h.Name != "package/package.json" || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxPackageJSON {
			return nil, fmt.Errorf("package.json is %d bytes, beyond %d", h.Size, maxPackageJSON)
		}
		var m map[string]any
		if err := json.NewDecoder(io.LimitReader(tr, maxPackageJSON)).Decode(&m); err != nil {
			return nil, fmt.Errorf("package.json: %w", err)
		}
		return m, nil
	}
}

// remove deletes the version as a component, so it leaves the packument.
func (npm) remove(ctx context.Context, c *client, key string) error {
	k, err := parseNPM(key)
	if err != nil {
		return err
	}
	q := url.Values{"repository": {c.repo}, "format": {"npm"}, "name": {k.base}, "version": {k.version}}
	if k.scope != "" {
		q.Set("group", k.scope)
	}
	if _, err := (npm{}).resolve(ctx, c, key); errors.Is(err, plugin.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	ids, err := c.searchFound(ctx, "components", q, nil)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.deleteURL(ctx, c.base.JoinPath("service/rest/v1/components", id).String(), key); err != nil {
			return err
		}
	}
	return nil
}
