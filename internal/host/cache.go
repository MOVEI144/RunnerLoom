package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

const CacheSafetyReserveGiB int64 = 2

type CacheProtection struct {
	Digests  map[string][]string `json:"digests"`
	Safe     bool                `json:"safe"`
	Warnings []string            `json:"warnings,omitempty"`
}

type ImageCacheSpec struct {
	Scope          string
	StateDir       string
	CacheDir       string
	DiskDir        string
	ProcessLock    string
	LimitGiB       int64
	Exec           Executor
	Protection     CacheProtection
	ProtectionFunc func(context.Context) (CacheProtection, error)
	Verify         bool
	Now            func() time.Time
	removePath     func(string) error
	syncDir        func(string) error
}

type ImageCacheEntry struct {
	Digest            string    `json:"digest,omitempty"`
	Kind              string    `json:"kind"`
	CachePath         string    `json:"cachePath,omitempty"`
	BasePath          string    `json:"basePath,omitempty"`
	SizeBytes         int64     `json:"sizeBytes"`
	ModifiedAt        time.Time `json:"modifiedAt"`
	CachePresent      bool      `json:"cachePresent"`
	BasePresent       bool      `json:"basePresent"`
	BaseSharesStorage bool      `json:"baseSharesStorage"`
	Verification      string    `json:"verification"`
	VerificationError string    `json:"verificationError,omitempty"`
	Protected         bool      `json:"protected"`
	ProtectionReasons []string  `json:"protectionReasons,omitempty"`
	Reclaimable       bool      `json:"reclaimable"`
	LinkCount         uint64    `json:"linkCount"`
	LinksRemoved      uint64    `json:"linksRemoved"`
	ReclaimableBytes  int64     `json:"reclaimablePhysicalBytes"`
	BlockedReasons    []string  `json:"blockedReasons,omitempty"`
}

type ImageCacheReport struct {
	Scope                     string            `json:"scope"`
	CacheDir                  string            `json:"cacheDir"`
	DiskDir                   string            `json:"diskDir,omitempty"`
	LimitGiB                  int64             `json:"limitGiB"`
	SafetyReserveGiB          int64             `json:"safetyReserveGiB"`
	CacheBytes                int64             `json:"cacheBytes"`
	PartialBytes              int64             `json:"partialBytes"`
	QuarantineBytes           int64             `json:"quarantineBytes"`
	BaseOnlyBytes             int64             `json:"baseOnlyBytes"`
	FilesystemFreeBytes       int64             `json:"filesystemFreeBytes"`
	AvailableForNewImageBytes int64             `json:"availableForNewImageBytes"`
	SafeToPrune               bool              `json:"safeToPrune"`
	Warnings                  []string          `json:"warnings,omitempty"`
	Entries                   []ImageCacheEntry `json:"entries"`
}

type ImageCachePruneReport struct {
	ImageCacheReport
	Applied             bool      `json:"applied"`
	Completed           bool      `json:"completed"`
	Cutoff              time.Time `json:"cutoff"`
	Removed             []string  `json:"removed,omitempty"`
	RemovedLogicalBytes int64     `json:"removedLogicalBytes"`
	ReclaimedBytes      int64     `json:"reclaimedPhysicalBytes"`
}

type ImageCacheSeedReport struct {
	Digest       string `json:"digest"`
	Source       string `json:"source"`
	Destination  string `json:"destination"`
	SizeBytes    int64  `json:"sizeBytes"`
	Deduplicated bool   `json:"deduplicated"`
	AlreadyThere bool   `json:"alreadyThere"`
}

func normalizeProtection(p CacheProtection) CacheProtection {
	if p.Digests == nil {
		p.Digests = map[string][]string{}
	}
	for digest, reasons := range p.Digests {
		seen := map[string]bool{}
		out := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			if reason != "" && !seen[reason] {
				seen[reason] = true
				out = append(out, reason)
			}
		}
		sort.Strings(out)
		p.Digests[digest] = out
	}
	return p
}

func addProtection(p *CacheProtection, digest, reason string) {
	if _, err := imageName(digest); err != nil {
		p.Safe = false
		p.Warnings = append(p.Warnings, fmt.Sprintf("invalid protected image digest %q", digest))
		return
	}
	if p.Digests == nil {
		p.Digests = map[string][]string{}
	}
	p.Digests[digest] = append(p.Digests[digest], reason)
}

func digestFromImageName(name string) (string, bool) {
	if len(name) != 64+len(".qcow2") || !strings.HasSuffix(name, ".qcow2") {
		return "", false
	}
	hex := strings.TrimSuffix(name, ".qcow2")
	if strings.Trim(hex, "0123456789abcdef") != "" {
		return "", false
	}
	return "sha256:" + hex, true
}

func digestFromPartialName(name string) (string, bool) {
	const prefix = ".partial-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return digestFromImageName(strings.TrimPrefix(name, prefix))
}

func digestFromQuarantineName(name string) (string, bool) {
	const prefix = ".quarantine-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	if len(rest) <= 65 || rest[64] != '-' {
		return "", false
	}
	hex := rest[:64]
	suffix := rest[65:]
	if suffix == "" || strings.Trim(hex, "0123456789abcdef") != "" {
		return "", false
	}
	for _, r := range suffix {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return "", false
		}
	}
	return "sha256:" + hex, true
}

func appendWarning(p *CacheProtection, message string) {
	p.Safe = false
	p.Warnings = append(p.Warnings, message)
}

// NodeCacheProtection derives fail-closed protection from the controller catalog,
// durable instance manifests and every qcow2 overlay found in the dedicated VM
// storage directory. A warning disables applied pruning but status remains useful.
func NodeCacheProtection(ctx context.Context, stateDir, diskDir, cluster, node string, exec Executor) CacheProtection {
	p := CacheProtection{Digests: map[string][]string{}, Safe: true}
	catalogPath := filepath.Join(stateDir, "catalog.json")
	if b, err := core.ReadSecret(catalogPath); err == nil {
		var catalog []core.Image
		if err = core.Decode(strings.NewReader(string(b)), &catalog); err != nil {
			appendWarning(&p, "catalog.json is invalid; applied pruning is disabled")
		} else {
			for _, image := range catalog {
				addProtection(&p, image.Digest, "controller-catalog:"+image.Name)
			}
		}
	} else if os.IsNotExist(err) {
		appendWarning(&p, "catalog.json is missing; run the Agent successfully before applied pruning")
	} else {
		appendWarning(&p, "catalog.json cannot be read safely: "+err.Error())
	}

	instancesDir := filepath.Join(stateDir, "instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil && !os.IsNotExist(err) {
		appendWarning(&p, "instance manifests cannot be listed: "+err.Error())
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			appendWarning(&p, "unexpected entry in instance manifest directory: "+entry.Name())
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !core.ValidID(id) {
			appendWarning(&p, "invalid instance manifest name: "+entry.Name())
			continue
		}
		b, readErr := core.ReadSecret(filepath.Join(instancesDir, entry.Name()))
		if readErr != nil {
			appendWarning(&p, "instance manifest cannot be read safely: "+entry.Name())
			continue
		}
		var m manifest
		if decodeErr := core.Decode(strings.NewReader(string(b)), &m); decodeErr != nil || m.Instance.ID != id {
			appendWarning(&p, "instance manifest is invalid: "+entry.Name())
			continue
		}
		if m.Phase != "deleted" {
			addProtection(&p, m.Instance.Image.Digest, "instance:"+id+":"+m.Phase)
		}
	}

	storage, err := os.Lstat(diskDir)
	if os.IsNotExist(err) {
		appendWarning(&p, "VM storage is missing; mount or initialize the approved disk directory before applied pruning")
		return normalizeProtection(p)
	}
	if err != nil || !storage.IsDir() || storage.Mode()&os.ModeSymlink != 0 {
		appendWarning(&p, "VM storage cannot be inspected safely")
		return normalizeProtection(p)
	}
	markerPath := filepath.Join(diskDir, ".runnerloom-owner")
	markerInfo, markerErr := regularInfo(markerPath)
	if markerErr != nil || markerInfo.Mode().Perm()&0022 != 0 {
		appendWarning(&p, "VM storage ownership marker is missing or unsafe")
		return normalizeProtection(p)
	}
	marker, markerErr := os.ReadFile(markerPath)
	if markerErr != nil || string(marker) != cluster+"/"+node {
		appendWarning(&p, "VM storage ownership marker does not match this Node")
		return normalizeProtection(p)
	}
	storageEntries, err := os.ReadDir(diskDir)
	if err != nil {
		appendWarning(&p, "VM storage cannot be listed: "+err.Error())
		return normalizeProtection(p)
	}
	baseDir := filepath.Join(diskDir, "base")
	for _, entry := range storageEntries {
		name := entry.Name()
		if name == ".runnerloom-owner" {
			continue
		}
		if name == "base" {
			st, statErr := os.Lstat(filepath.Join(diskDir, name))
			if statErr != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				appendWarning(&p, "owned base image path is not a safe directory")
			}
			continue
		}
		if !entry.IsDir() || !core.ValidID(name) {
			appendWarning(&p, "unexpected entry in dedicated VM storage: "+name)
			continue
		}
		vmDir := filepath.Join(diskDir, name)
		vmEntries, listErr := os.ReadDir(vmDir)
		if listErr != nil {
			appendWarning(&p, "VM directory cannot be listed safely: "+name)
			continue
		}
		rootSeen := false
		for _, vmEntry := range vmEntries {
			vmPath := filepath.Join(vmDir, vmEntry.Name())
			switch vmEntry.Name() {
			case "root.qcow2":
				rootSeen = true
				if st, statErr := os.Lstat(vmPath); statErr != nil || !st.Mode().IsRegular() {
					appendWarning(&p, "VM root overlay is not a regular file: "+name)
				}
			case "scratch.qcow2", "seed.iso":
				if st, statErr := os.Lstat(vmPath); statErr != nil || !st.Mode().IsRegular() {
					appendWarning(&p, "VM storage contains an unsafe managed file: "+name+"/"+vmEntry.Name())
				}
			case "serial.sock":
				if st, statErr := os.Lstat(vmPath); statErr != nil || st.Mode()&os.ModeSocket == 0 {
					appendWarning(&p, "VM serial endpoint is not a socket: "+name)
				}
			default:
				appendWarning(&p, "unexpected entry in VM directory: "+name+"/"+vmEntry.Name())
			}
		}
		if !rootSeen {
			appendWarning(&p, "VM directory has no root overlay and requires reconciliation: "+name)
			continue
		}
		root := filepath.Join(vmDir, "root.qcow2")
		st, statErr := os.Lstat(root)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !st.Mode().IsRegular() {
			appendWarning(&p, "VM root overlay cannot be inspected safely: "+name)
			continue
		}
		backing, inspectErr := overlayBacking(ctx, exec, root)
		if inspectErr != nil {
			appendWarning(&p, "VM root overlay inspection failed for "+name+": "+inspectErr.Error())
			continue
		}
		if !filepath.IsAbs(backing) || filepath.Clean(backing) != backing {
			appendWarning(&p, "VM root overlay has a non-absolute backing path: "+name)
			continue
		}
		rel, relErr := filepath.Rel(baseDir, backing)
		if relErr != nil || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.Dir(rel) != "." {
			appendWarning(&p, "VM root overlay points outside the owned base directory: "+name)
			continue
		}
		digest, ok := digestFromImageName(filepath.Base(backing))
		if !ok {
			appendWarning(&p, "VM root overlay has an unrecognized backing image: "+name)
			continue
		}
		addProtection(&p, digest, "overlay:"+name)
	}
	return normalizeProtection(p)
}

func overlayBacking(ctx context.Context, exec Executor, path string) (string, error) {
	if exec == nil {
		return "", errors.New("qemu-img executor is required")
	}
	b, err := exec.Run(ctx, "qemu-img", []string{"info", "--output=json", "-f", "qcow2", path}, nil)
	if err != nil {
		return "", err
	}
	var info struct {
		Format  string `json:"format"`
		Backing string `json:"backing-filename"`
		Data    string `json:"data-file"`
	}
	if err = json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	if info.Format != "qcow2" || info.Backing == "" || info.Data != "" {
		return "", errors.New("overlay is not a qcow2 image with one internal backing path")
	}
	return info.Backing, nil
}

func resolveProtection(ctx context.Context, spec ImageCacheSpec) (CacheProtection, error) {
	protection := spec.Protection
	if spec.ProtectionFunc != nil {
		var err error
		protection, err = spec.ProtectionFunc(ctx)
		if err != nil {
			return CacheProtection{}, err
		}
	}
	return normalizeProtection(protection), nil
}

func validateCacheSpec(spec ImageCacheSpec) (ImageCacheSpec, error) {
	if spec.Scope != "controller" && spec.Scope != "node" {
		return spec, errors.New("cache scope must be controller or node")
	}
	for _, path := range []string{spec.StateDir, spec.CacheDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return spec, errors.New("cache paths must be normalized absolute paths")
		}
	}
	if spec.Scope == "node" {
		if !filepath.IsAbs(spec.DiskDir) || filepath.Clean(spec.DiskDir) != spec.DiskDir || spec.DiskDir == "/" {
			return spec, errors.New("node cache requires a normalized absolute VM storage path")
		}
	}
	if spec.ProcessLock != "controller" && spec.ProcessLock != "agent" {
		return spec, errors.New("cache process lock must be controller or agent")
	}
	if spec.LimitGiB < 1 || spec.LimitGiB > 1048576 {
		return spec, errors.New("invalid image cache limit")
	}
	if spec.Exec == nil {
		return spec, errors.New("cache inspection requires an executor")
	}
	if spec.Now == nil {
		spec.Now = time.Now
	}
	if spec.removePath == nil {
		spec.removePath = os.Remove
	}
	if spec.syncDir == nil {
		spec.syncDir = syncDirectory
	}
	return spec, nil
}

func existingPrivateDir(path string) (bool, error) {
	exists := true
	for current := path; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if current == path {
				exists = false
			} else {
				return false, err
			}
		} else if err != nil {
			return false, err
		} else {
			if st.Mode()&os.ModeSymlink != 0 {
				return false, errors.New("symlink in private cache path")
			}
			if current == path && (!st.IsDir() || st.Mode().Perm()&0077 != 0) {
				return false, errors.New("cache directory must be owner-only")
			}
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return exists, nil
}

func regularInfo(path string) (os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("cache entry is not a regular file")
	}
	return st, nil
}

func fileLinkCount(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 0
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// InspectImageCache reports both logical cache use and the base links used by
// qcow2 overlays. It never modifies files. Full digest verification is opt-in.
func InspectImageCache(ctx context.Context, input ImageCacheSpec) (ImageCacheReport, error) {
	spec, err := validateCacheSpec(input)
	if err != nil {
		return ImageCacheReport{}, err
	}
	protection, err := resolveProtection(ctx, spec)
	if err != nil {
		return ImageCacheReport{}, err
	}
	spec.Protection = protection
	cacheExists, err := existingPrivateDir(spec.CacheDir)
	if err != nil {
		return ImageCacheReport{}, err
	}
	report := ImageCacheReport{
		Scope:            spec.Scope,
		CacheDir:         spec.CacheDir,
		DiskDir:          spec.DiskDir,
		LimitGiB:         spec.LimitGiB,
		SafetyReserveGiB: CacheSafetyReserveGiB,
		SafeToPrune:      spec.Protection.Safe,
		Warnings:         append([]string(nil), spec.Protection.Warnings...),
		Entries:          []ImageCacheEntry{},
	}

	byDigest := map[string]*ImageCacheEntry{}
	cacheEntries := []os.DirEntry{}
	if cacheExists {
		cacheEntries, err = os.ReadDir(spec.CacheDir)
		if err != nil {
			return report, err
		}
	}
	for _, entry := range cacheEntries {
		name := entry.Name()
		if name == "image-cache.lock" {
			continue
		}
		path := filepath.Join(spec.CacheDir, name)
		if digest, ok := digestFromImageName(name); ok {
			st, statErr := regularInfo(path)
			if statErr != nil {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, name+": "+statErr.Error())
				continue
			}
			item := &ImageCacheEntry{Digest: digest, Kind: "image", CachePath: path, SizeBytes: st.Size(), ModifiedAt: st.ModTime().UTC(), CachePresent: true, Verification: "not-checked", LinkCount: fileLinkCount(st)}
			item.ProtectionReasons = append(item.ProtectionReasons, spec.Protection.Digests[digest]...)
			item.Protected = len(item.ProtectionReasons) > 0
			if spec.Verify {
				images := &Images{Dir: spec.CacheDir, LimitGiB: spec.LimitGiB, Exec: spec.Exec}
				if verifyErr := images.Verify(ctx, digest); verifyErr != nil {
					item.Verification = "failed"
					item.VerificationError = verifyErr.Error()
					item.BlockedReasons = appendUnique(item.BlockedReasons, "verification-failed")
				} else {
					item.Verification = "ok"
				}
			}
			byDigest[digest] = item
			report.CacheBytes += st.Size()
			continue
		}
		if digest, ok := digestFromPartialName(name); ok {
			st, statErr := regularInfo(path)
			if statErr != nil {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, name+": "+statErr.Error())
				continue
			}
			item := ImageCacheEntry{Digest: digest, Kind: "partial", CachePath: path, SizeBytes: st.Size(), ModifiedAt: st.ModTime().UTC(), CachePresent: true, Verification: "incomplete", BlockedReasons: []string{"minimum-age-not-evaluated"}}
			report.Entries = append(report.Entries, item)
			report.CacheBytes += st.Size()
			report.PartialBytes += st.Size()
			continue
		}
		if digest, ok := digestFromQuarantineName(name); ok {
			st, statErr := regularInfo(path)
			if statErr != nil {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, name+": "+statErr.Error())
				continue
			}
			item := ImageCacheEntry{
				Digest:            digest,
				Kind:              "quarantine",
				CachePath:         path,
				SizeBytes:         st.Size(),
				ModifiedAt:        st.ModTime().UTC(),
				CachePresent:      true,
				Verification:      "failed",
				VerificationError: "digest mismatch; retained for inspection",
				LinkCount:         fileLinkCount(st),
				LinksRemoved:      1,
				BlockedReasons:    []string{"minimum-age-not-evaluated"},
			}
			report.Entries = append(report.Entries, item)
			report.CacheBytes += st.Size()
			report.QuarantineBytes += st.Size()
			continue
		}
		if strings.HasPrefix(name, ".import-") || strings.HasPrefix(name, ".seed-") {
			st, statErr := regularInfo(path)
			if statErr != nil {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, name+": "+statErr.Error())
				continue
			}
			report.Entries = append(report.Entries, ImageCacheEntry{Kind: "abandoned-import", CachePath: path, SizeBytes: st.Size(), ModifiedAt: st.ModTime().UTC(), CachePresent: true, Verification: "incomplete", BlockedReasons: []string{"minimum-age-not-evaluated"}})
			report.CacheBytes += st.Size()
			report.PartialBytes += st.Size()
			continue
		}
		report.SafeToPrune = false
		report.Warnings = append(report.Warnings, "unexpected cache entry: "+name)
	}

	if spec.Scope == "node" {
		baseDir := filepath.Join(spec.DiskDir, "base")
		baseEntries := []os.DirEntry{}
		baseInfo, statErr := os.Lstat(baseDir)
		if statErr == nil {
			if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, "base image path is not a safe directory")
			} else {
				baseEntries, err = os.ReadDir(baseDir)
				if err != nil {
					report.SafeToPrune = false
					report.Warnings = append(report.Warnings, "base image directory cannot be listed: "+err.Error())
				}
			}
		} else if !os.IsNotExist(statErr) {
			report.SafeToPrune = false
			report.Warnings = append(report.Warnings, "base image path cannot be inspected: "+statErr.Error())
		}
		for _, entry := range baseEntries {
			digest, ok := digestFromImageName(entry.Name())
			if !ok {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, "unexpected base image entry: "+entry.Name())
				continue
			}
			path := filepath.Join(baseDir, entry.Name())
			st, statErr := regularInfo(path)
			if statErr != nil {
				report.SafeToPrune = false
				report.Warnings = append(report.Warnings, entry.Name()+": "+statErr.Error())
				continue
			}
			item := byDigest[digest]
			if item == nil {
				item = &ImageCacheEntry{Digest: digest, Kind: "image", SizeBytes: st.Size(), ModifiedAt: st.ModTime().UTC(), Verification: "not-checked", LinkCount: fileLinkCount(st)}
				item.ProtectionReasons = append(item.ProtectionReasons, spec.Protection.Digests[digest]...)
				item.Protected = len(item.ProtectionReasons) > 0
				byDigest[digest] = item
				report.BaseOnlyBytes += st.Size()
			}
			item.BasePath = path
			item.BasePresent = true
			item.ModifiedAt = minTime(item.ModifiedAt, st.ModTime().UTC())
			if item.CachePresent {
				cacheInfo, infoErr := regularInfo(item.CachePath)
				if infoErr != nil || !os.SameFile(cacheInfo, st) {
					item.BlockedReasons = appendUnique(item.BlockedReasons, "base-cache-storage-mismatch")
					report.SafeToPrune = false
					report.Warnings = append(report.Warnings, "base image is not the cache hard link: "+digest)
				} else {
					item.BaseSharesStorage = true
				}
			}
		}
	}

	for digest, reasons := range spec.Protection.Digests {
		if item := byDigest[digest]; item != nil {
			item.ProtectionReasons = append(item.ProtectionReasons, reasons...)
			item.ProtectionReasons = normalizeProtection(CacheProtection{Digests: map[string][]string{digest: item.ProtectionReasons}}).Digests[digest]
			item.Protected = true
		}
	}
	for _, item := range byDigest {
		if item.Protected {
			item.BlockedReasons = appendUnique(item.BlockedReasons, "protected")
		}
		if item.CachePresent && item.BasePresent && !item.BaseSharesStorage {
			item.BlockedReasons = appendUnique(item.BlockedReasons, "base-cache-storage-mismatch")
		}
		item.LinksRemoved = 0
		if item.CachePresent {
			item.LinksRemoved++
		}
		if item.BasePresent {
			item.LinksRemoved++
		}
		item.Reclaimable = len(item.BlockedReasons) == 0
		if item.Reclaimable && (item.LinkCount == 0 || item.LinkCount <= item.LinksRemoved) {
			item.ReclaimableBytes = item.SizeBytes
		}
		report.Entries = append(report.Entries, *item)
	}

	freePath := spec.CacheDir
	if !cacheExists {
		freePath = spec.StateDir
	}
	freeBytes, freeErr := filesystemFreeBytes(freePath)
	if freeErr != nil {
		return report, freeErr
	}
	report.FilesystemFreeBytes = freeBytes
	budget := spec.LimitGiB*(1<<30) - report.CacheBytes
	disk := freeBytes - CacheSafetyReserveGiB*(1<<30)
	if budget < 0 {
		budget = 0
	}
	if disk < 0 {
		disk = 0
	}
	if budget < disk {
		report.AvailableForNewImageBytes = budget
	} else {
		report.AvailableForNewImageBytes = disk
	}
	sort.Strings(report.Warnings)
	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Kind != report.Entries[j].Kind {
			return report.Entries[i].Kind < report.Entries[j].Kind
		}
		if report.Entries[i].Digest != report.Entries[j].Digest {
			return report.Entries[i].Digest < report.Entries[j].Digest
		}
		return report.Entries[i].CachePath < report.Entries[j].CachePath
	})
	return report, nil
}

func markPruneCandidates(report *ImageCacheReport, cutoff time.Time) {
	for index := range report.Entries {
		entry := &report.Entries[index]
		entry.BlockedReasons = removeString(entry.BlockedReasons, "minimum-age-not-evaluated")
		if entry.ModifiedAt.After(cutoff) {
			entry.BlockedReasons = appendUnique(entry.BlockedReasons, "minimum-age")
		}
		entry.Reclaimable = len(entry.BlockedReasons) == 0 && !entry.Protected
		entry.ReclaimableBytes = 0
		if entry.Reclaimable && (entry.LinkCount == 0 || entry.LinkCount <= entry.LinksRemoved) {
			entry.ReclaimableBytes = entry.SizeBytes
		}
	}
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// quarantineDigestMismatch preserves a cache file whose bytes no longer match
// its content-addressed name. The caller must hold the image-cache lock. The
// file is renamed atomically and remains visible to cache status/prune.
func (i *Images) quarantineDigestMismatch(path, digest string) (string, error) {
	name, err := imageName(digest)
	if err != nil {
		return "", err
	}
	if filepath.Clean(path) != filepath.Join(i.Dir, name) {
		return "", errors.New("refusing to quarantine a path outside the expected cache entry")
	}
	info, err := regularInfo(path)
	if err != nil {
		return "", err
	}
	if links := fileLinkCount(info); links > 1 {
		return "", core.Fail(
			"CACHE_IMAGE_SHARED_CORRUPTION",
			"digest不一致のImageに別のhard linkがあります。所有serviceを停止し、overlay参照を確認してから整理してください",
			map[string]any{"path": path, "linkCount": links},
		)
	}
	actual, _, err := hashFile(path)
	if err != nil {
		return "", err
	}
	if actual == digest {
		return "", errors.New("refusing to quarantine an image whose digest now matches")
	}
	temp, err := os.CreateTemp(i.Dir, ".quarantine-"+digest[7:]+"-")
	if err != nil {
		return "", err
	}
	quarantine := temp.Name()
	if err = temp.Close(); err != nil {
		_ = os.Remove(quarantine)
		return "", err
	}
	if err = os.Remove(quarantine); err != nil {
		return "", err
	}
	if err = os.Rename(path, quarantine); err != nil {
		return "", err
	}
	if i.verified != nil {
		delete(i.verified, digest)
	}
	if err = syncDirectory(i.Dir); err != nil {
		return quarantine, err
	}
	return quarantine, nil
}

func pruneFailure(report ImageCachePruneReport, err error) (ImageCachePruneReport, error) {
	if !report.Applied {
		return report, err
	}
	return report, core.Fail(
		"CACHE_PRUNE_PARTIAL",
		"cache整理の一部が適用済みです。statusを再確認してから再実行してください",
		map[string]any{"report": report, "cause": err.Error()},
	)
}

// PruneImageCache is dry-run by default. Applied pruning requires the owning
// Controller or Agent lock, rescans under both process and cache locks, and only
// removes exact files that remain old and unreferenced.
func PruneImageCache(ctx context.Context, input ImageCacheSpec, olderThan time.Duration, apply bool) (ImageCachePruneReport, error) {
	if olderThan < 24*time.Hour {
		return ImageCachePruneReport{}, errors.New("cache prune minimum age must be at least 24 hours")
	}
	spec, err := validateCacheSpec(input)
	if err != nil {
		return ImageCachePruneReport{}, err
	}
	cutoff := spec.Now().UTC().Add(-olderThan)
	inspect := func() (ImageCachePruneReport, error) {
		report, inspectErr := InspectImageCache(ctx, spec)
		if inspectErr != nil {
			return ImageCachePruneReport{}, inspectErr
		}
		markPruneCandidates(&report, cutoff)
		return ImageCachePruneReport{ImageCacheReport: report, Cutoff: cutoff}, nil
	}
	report, err := inspect()
	if err != nil || !apply {
		return report, err
	}
	processLock, err := core.AcquireLock(spec.StateDir, spec.ProcessLock)
	if err != nil {
		return report, core.Fail("CACHE_OWNER_RUNNING", "cache prune前に所有するサービスを停止してください", spec.ProcessLock)
	}
	defer processLock.Close()
	cacheLock, err := core.AcquireLock(spec.CacheDir, "image-cache")
	if err != nil {
		return report, err
	}
	defer cacheLock.Close()
	report, err = inspect()
	if err != nil {
		return report, err
	}
	if !report.SafeToPrune {
		return report, core.Fail("CACHE_UNSAFE", "不明な参照またはファイルがあるためcacheを変更しません", report.Warnings)
	}
	for _, entry := range report.Entries {
		if !entry.Reclaimable {
			continue
		}
		for _, path := range []string{entry.BasePath, entry.CachePath} {
			if path == "" {
				continue
			}
			st, statErr := regularInfo(path)
			if statErr != nil {
				return pruneFailure(report, statErr)
			}
			if st.ModTime().UTC().After(cutoff) {
				return pruneFailure(report, errors.New("cache entry changed after prune planning"))
			}
			if removeErr := spec.removePath(path); removeErr != nil {
				return pruneFailure(report, removeErr)
			}
			report.Applied = true
			report.Removed = append(report.Removed, path)
			if syncErr := spec.syncDir(filepath.Dir(path)); syncErr != nil {
				return pruneFailure(report, syncErr)
			}
		}
		report.RemovedLogicalBytes += entry.SizeBytes
		report.ReclaimedBytes += entry.ReclaimableBytes
	}
	report.Applied = true
	report.Completed = true
	return report, nil
}

func filesystemFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	bsize := uint64(stat.Bsize)
	if bsize == 0 || stat.Bavail > uint64(math.MaxInt64)/bsize {
		return 0, errors.New("filesystem free-space value overflow")
	}
	return int64(stat.Bavail * bsize), nil
}

func cacheStoredBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var used int64
	for _, entry := range entries {
		name := entry.Name()
		_, final := digestFromImageName(name)
		_, partial := digestFromPartialName(name)
		_, quarantine := digestFromQuarantineName(name)
		if !final && !partial && !quarantine && !strings.HasPrefix(name, ".import-") && !strings.HasPrefix(name, ".seed-") {
			continue
		}
		st, statErr := entry.Info()
		if statErr != nil {
			return 0, statErr
		}
		if !st.Mode().IsRegular() {
			return 0, errors.New("cache contains a non-regular managed entry")
		}
		used += st.Size()
	}
	return used, nil
}

func cacheAvailableBytes(dir string, limitGiB, replacingBytes int64) (int64, error) {
	used, err := cacheStoredBytes(dir)
	if err != nil {
		return 0, err
	}
	free, err := filesystemFreeBytes(dir)
	if err != nil {
		return 0, err
	}
	budget := limitGiB*(1<<30) - (used - replacingBytes)
	disk := free + replacingBytes - CacheSafetyReserveGiB*(1<<30)
	if budget < disk {
		return budget, nil
	}
	return disk, nil
}

func cacheLogicalAvailableBytes(dir string, limitGiB, replacingBytes int64) (int64, error) {
	used, err := cacheStoredBytes(dir)
	if err != nil {
		return 0, err
	}
	available := limitGiB*(1<<30) - (used - replacingBytes)
	if available < 0 {
		return 0, nil
	}
	return available, nil
}

func lockCacheDirectories(first, second string) (func() error, error) {
	if first == second {
		lock, err := core.AcquireLock(first, "image-cache")
		if err != nil {
			return nil, err
		}
		return lock.Close, nil
	}
	ordered := []string{first, second}
	sort.Strings(ordered)
	one, err := core.AcquireLock(ordered[0], "image-cache")
	if err != nil {
		return nil, err
	}
	two, err := core.AcquireLock(ordered[1], "image-cache")
	if err != nil {
		one.Close()
		return nil, err
	}
	return func() error {
		errTwo := two.Close()
		errOne := one.Close()
		if errTwo != nil {
			return errTwo
		}
		return errOne
	}, nil
}

// SeedImageCache hard-links a verified Controller cache image into a local Node
// cache. It is an explicit single-host optimization and fails rather than
// silently copying when the caches are on different filesystems.
func SeedImageCache(ctx context.Context, sourceDir string, nodeSpec ImageCacheSpec, digest string) (ImageCacheSeedReport, error) {
	spec, err := validateCacheSpec(nodeSpec)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	if spec.Scope != "node" || spec.ProcessLock != "agent" {
		return ImageCacheSeedReport{}, errors.New("cache seed requires a node cache specification")
	}
	if !filepath.IsAbs(sourceDir) || filepath.Clean(sourceDir) != sourceDir || sourceDir == "/" {
		return ImageCacheSeedReport{}, errors.New("source cache must be a normalized absolute path")
	}
	if _, err = os.Stat(sourceDir); err != nil {
		return ImageCacheSeedReport{}, err
	}
	if err = core.PrivateDir(spec.CacheDir); err != nil {
		return ImageCacheSeedReport{}, err
	}
	processLock, err := core.AcquireLock(spec.StateDir, "agent")
	if err != nil {
		return ImageCacheSeedReport{}, core.Fail("CACHE_OWNER_RUNNING", "cache seed前にAgentサービスを停止してください", nil)
	}
	defer processLock.Close()
	unlock, err := lockCacheDirectories(sourceDir, spec.CacheDir)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	defer unlock()

	name, err := imageName(digest)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	source := filepath.Join(sourceDir, name)
	sourceInfo, err := regularInfo(source)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	actual, _, err := hashFile(source)
	if err != nil || actual != digest {
		return ImageCacheSeedReport{}, errors.New("source cache image digest mismatch")
	}
	images := &Images{Dir: sourceDir, LimitGiB: spec.LimitGiB, Exec: spec.Exec}
	if err = images.inspect(ctx, source); err != nil {
		return ImageCacheSeedReport{}, err
	}
	destination := filepath.Join(spec.CacheDir, name)
	if destInfo, statErr := regularInfo(destination); statErr == nil {
		if !os.SameFile(sourceInfo, destInfo) {
			actual, _, verifyErr := hashFile(destination)
			if verifyErr != nil || actual != digest {
				return ImageCacheSeedReport{}, errors.New("existing node cache image does not match source")
			}
		}
		return ImageCacheSeedReport{Digest: digest, Source: source, Destination: destination, SizeBytes: sourceInfo.Size(), Deduplicated: os.SameFile(sourceInfo, destInfo), AlreadyThere: true}, nil
	} else if !os.IsNotExist(statErr) {
		return ImageCacheSeedReport{}, statErr
	}
	available, err := cacheLogicalAvailableBytes(spec.CacheDir, spec.LimitGiB, 0)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	if sourceInfo.Size() > available {
		return ImageCacheSeedReport{}, errors.New("node cache logical budget is insufficient")
	}
	free, err := filesystemFreeBytes(spec.CacheDir)
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	if free <= CacheSafetyReserveGiB*(1<<30) {
		return ImageCacheSeedReport{}, errors.New("node cache filesystem safety reserve is exhausted")
	}
	temp, err := os.CreateTemp(spec.CacheDir, ".seed-")
	if err != nil {
		return ImageCacheSeedReport{}, err
	}
	tempPath := temp.Name()
	if err = temp.Close(); err != nil {
		os.Remove(tempPath)
		return ImageCacheSeedReport{}, err
	}
	if err = os.Remove(tempPath); err != nil {
		return ImageCacheSeedReport{}, err
	}
	defer os.Remove(tempPath)
	if err = os.Link(source, tempPath); err != nil {
		return ImageCacheSeedReport{}, fmt.Errorf("source and Node cache must share a filesystem for deduplicated seed: %w", err)
	}
	if err = os.Rename(tempPath, destination); err != nil {
		return ImageCacheSeedReport{}, err
	}
	if err = syncDirectory(spec.CacheDir); err != nil {
		return ImageCacheSeedReport{}, err
	}
	return ImageCacheSeedReport{Digest: digest, Source: source, Destination: destination, SizeBytes: sourceInfo.Size(), Deduplicated: true}, nil
}
