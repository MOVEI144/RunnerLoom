package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type cacheExecutor struct {
	backing map[string]string
}

func (e cacheExecutor) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	if name != "qemu-img" || len(args) == 0 || args[0] != "info" {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	path := args[len(args)-1]
	if backing := e.backing[path]; backing != "" {
		return json.Marshal(map[string]any{"format": "qcow2", "virtual-size": 1 << 30, "backing-filename": backing})
	}
	return []byte(`{"format":"qcow2","virtual-size":1073741824}`), nil
}

func cacheFixture(t *testing.T) (ImageCacheSpec, string, string, time.Time) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	cache := filepath.Join(state, "images")
	disks := filepath.Join(root, "disks")
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{state, 0700}, {cache, 0700}, {disks, 0711}, {filepath.Join(disks, "base"), 0711}} {
		if err := os.MkdirAll(item.path, item.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(item.path, item.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := core.WritePrivate(filepath.Join(state, "catalog.json"), []byte("[]")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disks, ".runnerloom-owner"), []byte("home/node-a"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	spec := ImageCacheSpec{
		Scope:       "node",
		StateDir:    state,
		CacheDir:    cache,
		DiskDir:     disks,
		ProcessLock: "agent",
		LimitGiB:    10,
		Exec:        cacheExecutor{backing: map[string]string{}},
		Now:         func() time.Time { return now },
	}
	spec.ProtectionFunc = func(ctx context.Context) (CacheProtection, error) {
		return NodeCacheProtection(ctx, state, disks, "home", "node-a", spec.Exec), nil
	}
	return spec, state, disks, now
}

func writeCachedImage(t *testing.T, spec ImageCacheSpec, data []byte, age time.Duration) (string, string) {
	t.Helper()
	digest := "sha256:" + core.Hash(data)
	name, err := imageName(digest)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(spec.CacheDir, name)
	if err = os.WriteFile(cachePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	stamp := spec.Now().Add(-age)
	if err = os.Chtimes(cachePath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return digest, cachePath
}

func TestImageCacheStatusProtectsCatalogImage(t *testing.T) {
	spec, state, _, _ := cacheFixture(t)
	digest, _ := writeCachedImage(t, spec, []byte("catalog-image"), 30*24*time.Hour)
	catalog, _ := json.Marshal([]core.Image{{Name: "ubuntu", Digest: digest, MinimumRootGiB: 1}})
	if err := core.WritePrivate(filepath.Join(state, "catalog.json"), catalog); err != nil {
		t.Fatal(err)
	}
	report, err := InspectImageCache(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != 1 || !report.Entries[0].Protected || report.Entries[0].Reclaimable {
		t.Fatalf("catalog image not protected: %+v", report)
	}
	if !strings.Contains(strings.Join(report.Entries[0].ProtectionReasons, " "), "controller-catalog") {
		t.Fatal("catalog protection reason missing")
	}
}

func TestImageCachePruneRemovesOnlyOldUnreferencedHardlinks(t *testing.T) {
	spec, _, disks, _ := cacheFixture(t)
	digest, cachePath := writeCachedImage(t, spec, []byte("old-unreferenced"), 30*24*time.Hour)
	name, _ := imageName(digest)
	basePath := filepath.Join(disks, "base", name)
	if err := os.Link(cachePath, basePath); err != nil {
		t.Fatal(err)
	}

	dry, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || len(dry.Entries) != 1 || !dry.Entries[0].Reclaimable {
		t.Fatalf("unexpected dry-run: %+v", dry)
	}
	if _, err = os.Stat(cachePath); err != nil {
		t.Fatal("dry-run changed cache", err)
	}

	applied, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || len(applied.Removed) != 2 || applied.ReclaimedBytes == 0 {
		t.Fatalf("unexpected applied report: %+v", applied)
	}
	for _, path := range []string{cachePath, basePath} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale path remains: %s (%v)", path, err)
		}
	}
}

func TestImageCachePruneProtectsActiveInstanceAndOverlay(t *testing.T) {
	spec, state, disks, _ := cacheFixture(t)
	digest, cachePath := writeCachedImage(t, spec, []byte("active-image"), 30*24*time.Hour)
	name, _ := imageName(digest)
	basePath := filepath.Join(disks, "base", name)
	if err := os.Link(cachePath, basePath); err != nil {
		t.Fatal(err)
	}
	instance := fixtureInstance()
	instance.Image.Digest = digest
	instance.Image.MinimumRootGiB = 1
	instance.ID = core.ID()
	vmDir := filepath.Join(disks, instance.ID)
	if err := os.Mkdir(vmDir, 0711); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(vmDir, "root.qcow2")
	if err := os.WriteFile(root, []byte("overlay"), 0600); err != nil {
		t.Fatal(err)
	}
	exec := cacheExecutor{backing: map[string]string{root: basePath}}
	spec.Exec = exec
	spec.ProtectionFunc = func(ctx context.Context) (CacheProtection, error) {
		return NodeCacheProtection(ctx, state, disks, "home", "node-a", exec), nil
	}
	manifestBytes, _ := json.Marshal(manifest{Instance: instance, Phase: "running"})
	if err := core.WritePrivate(filepath.Join(state, "instances", instance.ID+".json"), manifestBytes); err != nil {
		t.Fatal(err)
	}

	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != 1 || !report.Entries[0].Protected || report.Entries[0].Reclaimable {
		t.Fatalf("active image was reclaimable: %+v", report)
	}
	reasons := strings.Join(report.Entries[0].ProtectionReasons, " ")
	if !strings.Contains(reasons, "instance:") || !strings.Contains(reasons, "overlay:") {
		t.Fatalf("active protection evidence missing: %s", reasons)
	}
}

func TestImageCachePruneFailsClosedOnUnknownStorage(t *testing.T) {
	spec, _, disks, _ := cacheFixture(t)
	_, cachePath := writeCachedImage(t, spec, []byte("keep-on-unknown"), 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(disks, "foreign-file"), []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err == nil || report.SafeToPrune {
		t.Fatalf("unknown storage did not block prune: %+v %v", report, err)
	}
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Fatal("fail-closed prune removed cache", statErr)
	}
}

func TestImageCachePruneRejectsAnotherNodesStorage(t *testing.T) {
	spec, _, disks, _ := cacheFixture(t)
	_, cachePath := writeCachedImage(t, spec, []byte("foreign-owner"), 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(disks, ".runnerloom-owner"), []byte("home/node-b"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err == nil || report.SafeToPrune {
		t.Fatalf("foreign storage ownership did not block prune: %+v %v", report, err)
	}
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Fatal("foreign-owner check removed cache", statErr)
	}
}

func TestImageCachePruneRequiresAgentOffline(t *testing.T) {
	spec, state, _, _ := cacheFixture(t)
	_, cachePath := writeCachedImage(t, spec, []byte("locked-image"), 30*24*time.Hour)
	lock, err := core.AcquireLock(state, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err = PruneImageCache(context.Background(), spec, 7*24*time.Hour, true); err == nil {
		t.Fatal("prune ran while agent lock was held")
	}
	if _, err = os.Stat(cachePath); err != nil {
		t.Fatal("busy prune removed cache", err)
	}
}

func TestSeedImageCacheUsesHardLink(t *testing.T) {
	spec, _, _, _ := cacheFixture(t)
	source := filepath.Join(t.TempDir(), "controller-images")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("seeded-image")
	digest := "sha256:" + core.Hash(data)
	name, _ := imageName(digest)
	sourcePath := filepath.Join(source, name)
	if err := os.WriteFile(sourcePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := SeedImageCache(context.Background(), source, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, _ := os.Stat(sourcePath)
	destInfo, _ := os.Stat(report.Destination)
	if !report.Deduplicated || !os.SameFile(sourceInfo, destInfo) {
		t.Fatalf("seed was not deduplicated: %+v", report)
	}
}

func TestPruneReportsNoPhysicalReclaimWhileExternalHardLinkRemains(t *testing.T) {
	spec, _, disks, now := cacheFixture(t)
	source := filepath.Join(t.TempDir(), "controller-images")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("shared-single-host-image")
	digest := "sha256:" + core.Hash(data)
	name, _ := imageName(digest)
	sourcePath := filepath.Join(source, name)
	if err := os.WriteFile(sourcePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	seed, err := SeedImageCache(context.Background(), source, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(disks, "base", name)
	if err = os.Link(seed.Destination, basePath); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * 24 * time.Hour)
	if err = os.Chtimes(seed.Destination, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedLogicalBytes != int64(len(data)) || report.ReclaimedBytes != 0 {
		t.Fatalf("external link accounting is wrong: %+v", report)
	}
	if _, err = os.Stat(sourcePath); err != nil {
		t.Fatal("controller hard link was removed", err)
	}
}

func TestImageDownloadResumesRetainedPartial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	data := bytes.Repeat([]byte("runnerloom-range-data"), 1<<14)
	digest := "sha256:" + core.Hash(data)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("ETag", `"`+digest+`"`)
		if requests == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:len(data)/2])
			return
		}
		if r.Header.Get("Range") == "" || r.Header.Get("If-Range") != `"`+digest+`"` {
			t.Errorf("resume headers missing: %#v", r.Header)
		}
		http.ServeContent(w, r, "image.qcow2", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	images := &Images{Dir: dir, LimitGiB: 10, Exec: cacheExecutor{backing: map[string]string{}}}
	first, err := images.Download(context.Background(), server.Client(), server.URL, digest)
	if !errors.Is(err, io.ErrUnexpectedEOF) || !first.PartialRetain {
		t.Fatalf("first transfer did not retain resumable partial: %+v %v", first, err)
	}
	second, err := images.Download(context.Background(), server.Client(), server.URL, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || second.ResumedFrom == 0 || requests != 2 {
		t.Fatalf("transfer did not resume: %+v requests=%d", second, requests)
	}
	actual, err := os.ReadFile(second.Path)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("published image differs", err)
	}
}

func TestParseContentRange(t *testing.T) {
	value, err := parseContentRange("bytes 10-19/20")
	if err != nil || value.Start != 10 || value.End != 19 || value.Total != 20 {
		t.Fatal(value, err)
	}
	for _, bad := range []string{"", "bytes */20", "bytes 20-10/30", "bytes 0-20/20"} {
		if _, err = parseContentRange(bad); err == nil {
			t.Fatal("invalid range accepted", bad)
		}
	}
}

func TestImageCachePruneFailsClosedWhenVMStorageIsMissing(t *testing.T) {
	spec, _, disks, _ := cacheFixture(t)
	_, cachePath := writeCachedImage(t, spec, []byte("missing-storage-image"), 30*24*time.Hour)
	if err := os.RemoveAll(disks); err != nil {
		t.Fatal(err)
	}
	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err == nil || report.SafeToPrune {
		t.Fatalf("missing VM storage did not block prune: %+v %v", report, err)
	}
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Fatal("fail-closed prune removed cache", statErr)
	}
}

func TestImageCachePruneReportsPartialApplication(t *testing.T) {
	spec, _, disks, _ := cacheFixture(t)
	digest, cachePath := writeCachedImage(t, spec, []byte("partial-prune-image"), 30*24*time.Hour)
	name, _ := imageName(digest)
	basePath := filepath.Join(disks, "base", name)
	if err := os.Link(cachePath, basePath); err != nil {
		t.Fatal(err)
	}
	calls := 0
	spec.removePath = func(path string) error {
		calls++
		if calls == 2 {
			return errors.New("injected second unlink failure")
		}
		return os.Remove(path)
	}
	report, err := PruneImageCache(context.Background(), spec, 7*24*time.Hour, true)
	if err == nil || !report.Applied || report.Completed || len(report.Removed) != 1 {
		t.Fatalf("partial prune was not reported accurately: %+v %v", report, err)
	}
	var fault *core.Error
	if !errors.As(err, &fault) || fault.Code != "CACHE_PRUNE_PARTIAL" {
		t.Fatalf("wrong partial prune error: %T %v", err, err)
	}
	if _, statErr := os.Stat(basePath); !os.IsNotExist(statErr) {
		t.Fatalf("first unlink was not applied: %v", statErr)
	}
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Fatal("failed second unlink unexpectedly removed cache", statErr)
	}
}

func TestImageDownloadQuarantinesCorruptFinalAndRecovers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	valid := bytes.Repeat([]byte("valid-runnerloom-image"), 4096)
	corrupt := []byte("corrupt-cache-image")
	digest := "sha256:" + core.Hash(valid)
	name, _ := imageName(digest)
	final := filepath.Join(dir, name)
	if err := os.WriteFile(final, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("download did not force identity encoding: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("ETag", `"`+digest+`"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(valid)))
		_, _ = w.Write(valid)
	}))
	defer server.Close()

	images := &Images{Dir: dir, LimitGiB: 10, Exec: cacheExecutor{backing: map[string]string{}}}
	report, err := images.Download(context.Background(), server.Client(), server.URL, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.QuarantinedPath == "" {
		t.Fatalf("corrupt final was not quarantined before recovery: %+v", report)
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatal("recovered final image differs", err)
	}
	preserved, err := os.ReadFile(report.QuarantinedPath)
	if err != nil || !bytes.Equal(preserved, corrupt) {
		t.Fatal("quarantine did not preserve corrupt bytes", err)
	}

	spec := ImageCacheSpec{
		Scope:       "controller",
		StateDir:    filepath.Dir(dir),
		CacheDir:    dir,
		ProcessLock: "controller",
		LimitGiB:    10,
		Exec:        cacheExecutor{backing: map[string]string{}},
		Protection:  CacheProtection{Digests: map[string][]string{}, Safe: true},
	}
	status, err := InspectImageCache(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.QuarantineBytes != int64(len(corrupt)) {
		t.Fatalf("quarantine bytes not accounted: %+v", status)
	}
	found := false
	for _, entry := range status.Entries {
		if entry.Kind == "quarantine" && entry.Digest == digest {
			found = true
		}
	}
	if !found {
		t.Fatalf("quarantine entry missing from status: %+v", status)
	}
}

func TestImageImportQuarantinesCorruptExistingEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	valid := []byte("valid-import-image")
	digest := "sha256:" + core.Hash(valid)
	name, _ := imageName(digest)
	final := filepath.Join(dir, name)
	if err := os.WriteFile(final, []byte("wrong-existing-image"), 0600); err != nil {
		t.Fatal(err)
	}
	images := &Images{Dir: dir, LimitGiB: 10, Exec: cacheExecutor{backing: map[string]string{}}}
	path, err := images.Import(context.Background(), bytes.NewReader(valid), digest)
	if err != nil {
		t.Fatal(err)
	}
	if path != final {
		t.Fatalf("unexpected import path: %s", path)
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatal("valid import was not published", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".quarantine-"+digest[7:]+"-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt existing image was not quarantined: %v %v", matches, err)
	}
}

func TestSharedCorruptImageIsNotAutomaticallyQuarantined(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	expected := []byte("expected-image")
	digest := "sha256:" + core.Hash(expected)
	name, _ := imageName(digest)
	final := filepath.Join(dir, name)
	if err := os.WriteFile(final, []byte("corrupt-shared-image"), 0600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.Link(final, external); err != nil {
		t.Fatal(err)
	}
	images := &Images{Dir: dir, LimitGiB: 10, Exec: cacheExecutor{backing: map[string]string{}}}
	_, err := images.quarantineDigestMismatch(final, digest)
	var fault *core.Error
	if !errors.As(err, &fault) || fault.Code != "CACHE_IMAGE_SHARED_CORRUPTION" {
		t.Fatalf("shared corruption was not rejected safely: %T %v", err, err)
	}
	if _, statErr := os.Stat(final); statErr != nil {
		t.Fatal("shared cache link was renamed", statErr)
	}
	if _, statErr := os.Stat(external); statErr != nil {
		t.Fatal("external hard link was changed", statErr)
	}
}

func TestImageDownloadReportsHTTPStatusBeforeMissingETag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	digest := "sha256:" + core.Hash([]byte("not-served"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	images := &Images{Dir: dir, LimitGiB: 10, Exec: cacheExecutor{backing: map[string]string{}}}
	_, err := images.Download(context.Background(), server.Client(), server.URL, digest)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || strings.Contains(err.Error(), "ETag") {
		t.Fatalf("non-success response was misclassified: %v", err)
	}
}
