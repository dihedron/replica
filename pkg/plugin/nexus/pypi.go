package nexus

import (
	"context"
	"crypto/sha1" //nolint:gosec // Nexus's ETag
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/stream"
	"golang.org/x/net/html"
)

// maxSimpleIndex bounds a project's simple index (NX-11).
const maxSimpleIndex = 8 << 20

var (
	// PEP 427: name-version(-build)?-python-abi-platform.whl
	pypiWheel = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._]*[A-Za-z0-9])?)-([0-9][A-Za-z0-9.!+_]*)(?:-([0-9][A-Za-z0-9_.]*))?-([A-Za-z0-9_.]+)-([A-Za-z0-9_.]+)-([A-Za-z0-9_.]+)\.whl$`)
	// PEP 625: name-version.tar.gz
	pypiSdist = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._]*[A-Za-z0-9])?)-([0-9][A-Za-z0-9.!+_]*)\.tar\.gz$`)
	pep503Run = regexp.MustCompile(`[-_.]+`)
)

// pep503 is a project's normalised name.
func pep503(name string) string {
	return strings.ToLower(pep503Run.ReplaceAllString(name, "-"))
}

// pypiKey is a distribution's file name and what twine says of it.
type pypiKey struct {
	file, name, project, version, filetype, pyversion string
}

func parsePyPI(key string) (pypiKey, error) {
	if m := pypiWheel.FindStringSubmatch(key); m != nil {
		return pypiKey{file: key, name: m[1], project: pep503(m[1]), version: m[2], filetype: "bdist_wheel", pyversion: m[4]}, nil
	}
	if m := pypiSdist.FindStringSubmatch(key); m != nil {
		return pypiKey{file: key, name: m[1], project: pep503(m[1]), version: m[2], filetype: "sdist", pyversion: "source"}, nil
	}
	return pypiKey{}, plugin.Wrapf(plugin.Permanent, "nexus: %q is neither a wheel (PEP 427) nor an sdist (PEP 625) file name", key)
}

// pypi: the key is a distribution's file name.
type pypi struct{}

// resolve finds the file in the project's simple index (NX-6) with its
// sha256.
func (pypi) resolve(ctx context.Context, c *client, key string) (asset, error) {
	k, err := parsePyPI(key)
	if err != nil {
		return asset{}, err
	}
	index := c.root().String() + "simple/" + url.PathEscape(k.project) + "/"
	b, err := c.get(ctx, index, "the simple index of "+k.project, maxSimpleIndex)
	if err != nil {
		return asset{}, err
	}
	z := html.NewTokenizer(strings.NewReader(string(b)))
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return asset{}, fmt.Errorf("nexus: %s: %w", key, plugin.ErrNotFound)
			}
			return asset{}, plugin.Wrapf(plugin.Permanent, "nexus: the simple index of %s: %v", k.project, z.Err())
		case html.StartTagToken:
			name, attrs := z.TagName()
			if string(name) != "a" || !attrs {
				continue
			}
			for {
				attr, val, more := z.TagAttr()
				if string(attr) == "href" {
					if a, ok, err := c.pypiLink(index, string(val), k.file); ok || err != nil {
						return a, err
					}
				}
				if !more {
					break
				}
			}
		}
	}
}

// pypiLink is the link's asset if it names the file.
func (c *client) pypiLink(index, href, file string) (asset, bool, error) {
	u, err := url.Parse(href)
	if err != nil {
		return asset{}, false, nil //nolint:nilerr // another link
	}
	if name, err := url.PathUnescape(path.Base(u.EscapedPath())); err != nil || name != file {
		return asset{}, false, nil //nolint:nilerr // another link
	}
	target, err := c.inside(index, href)
	if err != nil {
		return asset{}, true, err
	}
	a := asset{url: target}
	if sum, ok := strings.CutPrefix(u.Fragment, "sha256="); ok {
		a.algo, a.sum = "sha256", strings.ToLower(sum)
	}
	return a, true, nil
}

// sink uploads as twine does: one multipart POST, streamed, the file's
// sha256 after it (NX-9).
func (pypi) sink(_ context.Context, c *client, key string, size int64) (*upload, error) {
	k, err := parsePyPI(key)
	if err != nil {
		return nil, err
	}
	s1, s256 := sha1.New(), sha256.New() //nolint:gosec // Nexus's ETag
	u := &upload{key: key}
	u.s = stream.New("nexus: upload of "+key, size, io.MultiWriter(s1, s256), func(ctx context.Context, body io.Reader, _ int64) error {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			_ = pw.CloseWithError(pypiForm(mw, k, body, s256))
		}()
		resp, err := c.do(ctx, http.MethodPost, c.root().String(), http.Header{"Content-Type": {mw.FormDataContentType()}}, pr, -1)
		_ = pr.CloseWithError(errors.New("the request ended")) // unblocks the form's writer
		if err != nil {
			return err
		}
		defer discard(resp)
		return uploaded(resp, key, "the file may be there already, and the repository not allow redeploying")
	})
	u.finish = func(ctx context.Context) (plugin.ObjectInfo, error) {
		a, err := pypi{}.resolve(ctx, c, key)
		if err != nil {
			return plugin.ObjectInfo{}, err
		}
		if a.algo != "" && a.sum != hex.EncodeToString(s256.Sum(nil)) {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Integrity, "nexus: %s: the index's sha256 is not that of the file sent", key)
		}
		return c.checkSHA1(ctx, a.url, key, hex.EncodeToString(s1.Sum(nil)))
	}
	return u, nil
}

// pypiForm writes twine's form: its fields, the file, then the file's
// sha256, which the stream's sum has seen once the body ends.
func pypiForm(mw *multipart.Writer, k pypiKey, body io.Reader, s256 interface{ Sum([]byte) []byte }) error {
	for _, f := range [][2]string{
		{":action", "file_upload"}, {"protocol_version", "1"}, {"metadata_version", "2.1"},
		{"name", k.name}, {"version", k.version}, {"filetype", k.filetype}, {"pyversion", k.pyversion},
	} {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("content", k.file)
	if err != nil {
		return err
	}
	if body != nil {
		if _, err := io.Copy(fw, body); err != nil {
			return err
		}
	}
	if err := mw.WriteField("sha256_digest", hex.EncodeToString(s256.Sum(nil))); err != nil {
		return err
	}
	return mw.Close()
}

// remove deletes the file and its PEP 658 metadata as assets: Nexus
// refuses a DELETE of a PyPI content path.
func (pypi) remove(ctx context.Context, c *client, key string) error {
	k, err := parsePyPI(key)
	if err != nil {
		return err
	}
	q := url.Values{"repository": {c.repo}, "format": {"pypi"}, "version": {k.version}}
	if _, err := (pypi{}).resolve(ctx, c, key); errors.Is(err, plugin.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	ids, err := c.searchFound(ctx, "assets", q, func(p string) bool {
		return strings.HasSuffix(p, "/"+k.file) || strings.HasSuffix(p, "/"+k.file+".metadata")
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.deleteURL(ctx, c.base.JoinPath("service/rest/v1/assets", id).String(), key); err != nil {
			return err
		}
	}
	return nil
}
