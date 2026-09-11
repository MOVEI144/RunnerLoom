package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type Images struct {
	Dir      string
	LimitGiB int64
	Exec     Executor
	mu       sync.Mutex
	verified map[string]imageStamp
}
type imageStamp struct {
	Size int64
	Mod  int64
}

var errImageDigestMismatch = errors.New("image digest mismatch")

func imageName(digest string) (string, error) {
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") || strings.Trim(digest[7:], "0123456789abcdef") != "" {
		return "", errors.New("image must have an exact SHA-256 digest")
	}
	return digest[7:] + ".qcow2", nil
}
func (i *Images) Path(digest string) (string, error) {
	name, e := imageName(digest)
	if e != nil {
		return "", e
	}
	return filepath.Join(i.Dir, name), nil
}
func hashFile(path string) (string, imageStamp, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return "", imageStamp{}, e
	}
	if !st.Mode().IsRegular() {
		return "", imageStamp{}, errors.New("image is not a regular file")
	}
	f, e := os.Open(path)
	if e != nil {
		return "", imageStamp{}, e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", imageStamp{}, e
	}
	after, e := f.Stat()
	if e != nil {
		return "", imageStamp{}, e
	}
	if st.Size() != after.Size() || st.ModTime() != after.ModTime() {
		return "", imageStamp{}, errors.New("image changed during verification")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), imageStamp{st.Size(), st.ModTime().UnixNano()}, nil
}
func (i *Images) Verify(ctx context.Context, digest string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	path, e := i.Path(digest)
	if e != nil {
		return e
	}
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("image is not regular")
	}
	stamp := imageStamp{st.Size(), st.ModTime().UnixNano()}
	if i.verified != nil && i.verified[digest] == stamp {
		return nil
	}
	actual, stamp, e := hashFile(path)
	if e != nil {
		return e
	}
	if actual != digest {
		return errImageDigestMismatch
	}
	if e = i.inspect(ctx, path); e != nil {
		return e
	}
	if i.verified == nil {
		i.verified = map[string]imageStamp{}
	}
	i.verified[digest] = stamp
	return nil
}
func (i *Images) inspect(ctx context.Context, path string) error {
	b, e := i.Exec.Run(ctx, "qemu-img", []string{"info", "--output=json", "-f", "qcow2", path}, nil)
	if e != nil {
		return e
	}
	var info struct {
		Format    string `json:"format"`
		Backing   string `json:"backing-filename"`
		Data      string `json:"data-file"`
		Encrypted bool   `json:"encrypted"`
		Virtual   int64  `json:"virtual-size"`
		Specific  struct {
			Data map[string]any `json:"data"`
		} `json:"format-specific"`
	}
	if e = json.Unmarshal(b, &info); e != nil {
		return e
	}
	if info.Format != "qcow2" || info.Backing != "" || info.Data != "" || info.Encrypted || info.Virtual <= 0 {
		return errors.New("only standalone unencrypted qcow2 images are accepted")
	}
	if _, ok := info.Specific.Data["data-file"]; ok {
		return errors.New("external qcow2 data files are forbidden")
	}
	return nil
}

// Import verifies the whole stream before publishing. It never evicts images
// automatically: deleting a backing image while an overlay exists is unsafe.
func (i *Images) Import(ctx context.Context, r io.Reader, digest string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	name, e := imageName(digest)
	if e != nil {
		return "", e
	}
	if i.LimitGiB < 1 || i.LimitGiB > 1048576 {
		return "", errors.New("invalid image cache limit")
	}
	if e = core.PrivateDir(i.Dir); e != nil {
		return "", e
	}
	lock, e := core.AcquireLock(i.Dir, "image-cache")
	if e != nil {
		return "", e
	}
	defer lock.Close()
	path := filepath.Join(i.Dir, name)
	if _, e = os.Lstat(path); e == nil {
		actual, _, e := hashFile(path)
		if e != nil {
			return "", e
		}
		if actual != digest {
			if _, e = i.quarantineDigestMismatch(path, digest); e != nil {
				return "", fmt.Errorf("existing image has incorrect digest and could not be quarantined: %w", e)
			}
		} else {
			return path, i.inspect(ctx, path)
		}
	} else if !os.IsNotExist(e) {
		return "", e
	}
	available, e := cacheAvailableBytes(i.Dir, i.LimitGiB, 0)
	if e != nil {
		return "", e
	}
	if available <= 0 {
		return "", errors.New("image cache capacity or filesystem safety reserve exhausted")
	}
	f, e := os.CreateTemp(i.Dir, ".import-")
	if e != nil {
		return "", e
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	written, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, available+1))
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return "", e
	}
	if written > available {
		return "", errors.New("image exceeds cache budget")
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return "", errors.New("downloaded image SHA-256 mismatch")
	}
	if e = i.inspect(ctx, f.Name()); e != nil {
		return "", e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return "", e
	}
	dir, e := os.Open(i.Dir)
	if e != nil {
		return "", e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return "", e
	}
	return path, nil
}
func (i *Images) Available(ctx context.Context, wanted []core.Image) ([]string, error) {
	out := []string{}
	for _, im := range wanted {
		path, e := i.Path(im.Digest)
		if e != nil {
			return nil, e
		}
		if _, e = os.Lstat(path); os.IsNotExist(e) {
			continue
		}
		if e = i.Verify(ctx, im.Digest); e != nil {
			return nil, fmt.Errorf("image %s failed verification: %w", im.Name, e)
		}
		out = append(out, im.Digest)
	}
	return out, nil
}
