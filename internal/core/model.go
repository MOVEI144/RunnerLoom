// Package core contains RunnerLoom's persisted, transport-independent contract.
package core

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const Version = "runnerloom/v1alpha1"
const MaxJSON = 1 << 20

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func ValidName(s string) bool { return namePattern.MatchString(s) }
func ValidID(s string) bool   { return idPattern.MatchString(s) }
func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Hash(b []byte) string     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Fingerprint(v any) string { b, _ := json.Marshal(v); return Hash(b) }

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string                 { return e.Code + ": " + e.Message }
func Fail(code, msg string, details any) error { return &Error{code, msg, details} }

type Resources struct {
	CPU    int64 `json:"vcpu"`
	Memory int64 `json:"memoryMiB"`
	Disk   int64 `json:"diskGiB"`
}

func (r Resources) Add(s Resources) Resources {
	return Resources{r.CPU + s.CPU, r.Memory + s.Memory, r.Disk + s.Disk}
}
func (r Resources) Sub(s Resources) Resources {
	return Resources{r.CPU - s.CPU, r.Memory - s.Memory, r.Disk - s.Disk}
}
func (r Resources) Mul(n int64) Resources { return Resources{r.CPU * n, r.Memory * n, r.Disk * n} }
func (r Resources) Fits(s Resources) bool {
	return r.CPU >= 0 && r.Memory >= 0 && r.Disk >= 0 && r.CPU <= s.CPU && r.Memory <= s.Memory && r.Disk <= s.Disk
}
func (r Resources) Empty() bool { return r == Resources{} }
func (r Resources) Valid() bool {
	return r.CPU > 0 && r.CPU <= 65536 && r.Memory >= 512 && r.Memory <= 1073741824 && r.Disk > 0 && r.Disk <= 1073741824
}

type GitHub struct {
	URL                 string   `json:"url"`
	RunnerGroupID       int      `json:"runnerGroupID"`
	CredentialFile      string   `json:"credentialFile"`
	AllowedRepositories []string `json:"allowedRepositories"`
}
type Image struct {
	Name           string `json:"name"`
	Digest         string `json:"digest"`
	MinimumRootGiB int64  `json:"minimumRootGiB"`
}
type Node struct {
	Name         string            `json:"name"`
	Budget       Resources         `json:"budget"`
	LocalCeiling Resources         `json:"localCeiling"`
	Labels       map[string]string `json:"labels,omitempty"`
	AllowedPools []string          `json:"allowedPools"`
}
type Pool struct {
	Name             string            `json:"name"`
	RunnerName       string            `json:"runnerName"`
	Image            string            `json:"image"`
	VCPU             int64             `json:"vcpu"`
	MemoryMiB        int64             `json:"memoryMiB"`
	OverheadMiB      int64             `json:"overheadMiB"`
	RootGiB          int64             `json:"rootGiB"`
	ScratchGiB       int64             `json:"scratchGiB"`
	DiskOverheadGiB  int64             `json:"diskOverheadGiB"`
	MaxRunners       int64             `json:"maxRunners"`
	WarmIdle         int64             `json:"warmIdle"`
	ExecutionMinutes int64             `json:"executionMinutes"`
	NodeSelector     map[string]string `json:"nodeSelector,omitempty"`
	NodeName         string            `json:"nodeName,omitempty"`
	Enabled          bool              `json:"enabled"`
	// Tasks marks a Pool that serves agent tasks from approved clients instead
	// of a GitHub scale set. omitempty keeps existing Pool fingerprints stable.
	Tasks bool `json:"tasks,omitempty"`
}

func (p Pool) Charge() Resources {
	return Resources{p.VCPU, p.MemoryMiB + p.OverheadMiB, p.RootGiB + p.ScratchGiB + p.DiskOverheadGiB}
}

type Reservation struct {
	Name  string `json:"name"`
	Node  string `json:"node"`
	Pool  string `json:"pool"`
	Slots int64  `json:"slots"`
}
type Config struct {
	APIVersion   string        `json:"apiVersion"`
	Name         string        `json:"name"`
	GitHub       GitHub        `json:"github"`
	Images       []Image       `json:"images"`
	Nodes        []Node        `json:"nodes"`
	Pools        []Pool        `json:"pools"`
	Reservations []Reservation `json:"reservations"`
}

// UsesGitHub reports whether any Pool is a GitHub scale-set Pool or a github
// block was given. Only then is the github block required and validated.
func (c Config) UsesGitHub() bool {
	if c.GitHub.URL != "" || c.GitHub.RunnerGroupID != 0 || c.GitHub.CredentialFile != "" || len(c.GitHub.AllowedRepositories) > 0 {
		return true
	}
	for _, p := range c.Pools {
		if !p.Tasks {
			return true
		}
	}
	return false
}

func (c Config) Pool(s string) (Pool, bool) {
	for _, p := range c.Pools {
		if p.Name == s {
			return p, true
		}
	}
	return Pool{}, false
}
func (c Config) Node(s string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.Name == s {
			return n, true
		}
	}
	return Node{}, false
}
func (c Config) Image(s string) (Image, bool) {
	for _, i := range c.Images {
		if i.Name == s {
			return i, true
		}
	}
	return Image{}, false
}
func (c Config) Eligible(p Pool, n Node) bool {
	if p.NodeName != "" && p.NodeName != n.Name {
		return false
	}
	allowed := false
	for _, s := range n.AllowedPools {
		if s == p.Name {
			allowed = true
		}
	}
	if !allowed {
		return false
	}
	for k, v := range p.NodeSelector {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}
func (c Config) Validate() error {
	bad := map[string]string{}
	check := func(ok bool, path, msg string) {
		if !ok {
			bad[path] = msg
		}
	}
	check(c.APIVersion == Version, "apiVersion", "対応しない設定版です")
	check(ValidName(c.Name), "name", "Cluster名は英小文字・数字・ハイフンで指定してください")
	// A cluster that only serves agent tasks may omit the github block.
	if c.UsesGitHub() {
		u, e := url.Parse(c.GitHub.URL)
		parts := []string{}
		if e == nil {
			parts = strings.Split(strings.Trim(u.Path, "/"), "/")
		}
		check(e == nil && u != nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(parts) >= 1 && len(parts) <= 2 && parts[0] != "", "github.url", "https://github.com/組織 または /所有者/repo を指定してください")
		check(c.GitHub.RunnerGroupID > 0, "github.runnerGroupID", "明示的なRunner Group IDが必要です")
		check(filepath.IsAbs(c.GitHub.CredentialFile) && filepath.Clean(c.GitHub.CredentialFile) == c.GitHub.CredentialFile, "github.credentialFile", "認証情報は絶対パスの別ファイルで指定してください")
		check(len(c.GitHub.AllowedRepositories) > 0, "github.allowedRepositories", "許可Repositoryを指定してください")
		repoSeen := map[string]bool{}
		for _, r := range c.GitHub.AllowedRepositories {
			rp := strings.Split(r, "/")
			ok := len(rp) == 2 && rp[0] != "" && rp[1] != "" && !strings.ContainsAny(r, " \\\t\r\n") && len(parts) > 0 && strings.EqualFold(rp[0], parts[0])
			if len(parts) == 2 {
				ok = ok && strings.EqualFold(r, strings.Join(parts, "/"))
			}
			check(ok && !repoSeen[strings.ToLower(r)], "github.allowedRepositories."+r, "対象外または重複したRepositoryです")
			repoSeen[strings.ToLower(r)] = true
		}
	}
	if len(c.Nodes) > 1024 || len(c.Pools) > 1024 || len(c.Images) > 1024 || len(c.Reservations) > 4096 {
		return Fail("CONFIG_LIMIT", "設定数が上限を超えています", nil)
	}
	images := map[string]bool{}
	nodes := map[string]bool{}
	pools := map[string]bool{}
	runnerNames := map[string]bool{}
	for _, i := range c.Images {
		check(ValidName(i.Name) && !images[i.Name], "images."+i.Name, "不正または重複した名前です")
		images[i.Name] = true
		check(digestPattern.MatchString(i.Digest), "images."+i.Name+".digest", "sha256:で固定してください")
		check(i.MinimumRootGiB > 0 && i.MinimumRootGiB <= 1048576, "images."+i.Name+".minimumRootGiB", "容量が範囲外です")
	}
	for _, n := range c.Nodes {
		check(ValidName(n.Name) && !nodes[n.Name], "nodes."+n.Name, "不正または重複したNode名です")
		nodes[n.Name] = true
		check(n.Budget.Valid() && n.LocalCeiling.Valid() && n.Budget.Fits(n.LocalCeiling), "nodes."+n.Name+".budget", "Node側の承認上限を超えています")
		for k, v := range n.Labels {
			check(ValidName(k) && ValidName(v), "nodes."+n.Name+".labels", "ラベルが不正です")
		}
	}
	for _, p := range c.Pools {
		key := "pools." + p.Name
		check(ValidName(p.Name) && !pools[p.Name], key, "不正または重複したPool名です")
		pools[p.Name] = true
		if p.Tasks {
			check(p.RunnerName == "" || ValidName(p.RunnerName) && !runnerNames[p.RunnerName], key+".runnerName", "不正または重複したGitHub選択名です")
			check(p.WarmIdle == 0, key+".warmIdle", "タスク用Poolは待機VMを持てません")
			// The host refuses to start a task with under 10 minutes left, and
			// the guest keeps a further 7 minutes for commit, push and result.
			check(p.ExecutionMinutes >= 30, key+".executionMinutes", "タスク用Poolの実行時間は30分以上にしてください")
		} else {
			check(ValidName(p.RunnerName) && !runnerNames[p.RunnerName], key+".runnerName", "不正または重複したGitHub選択名です")
		}
		if p.RunnerName != "" {
			runnerNames[p.RunnerName] = true
		}
		im, ok := c.Image(p.Image)
		check(ok, key+".image", "Imageが存在しません")
		check(p.VCPU >= 1 && p.VCPU <= 65536 && p.MemoryMiB >= 512 && p.MemoryMiB <= 1073741824 && p.OverheadMiB >= 512 && p.OverheadMiB <= 1048576, key+".size", "CPU・RAM・管理用余裕が範囲外です")
		check(p.RootGiB >= 1 && p.RootGiB >= im.MinimumRootGiB && p.RootGiB <= 1048576 && p.ScratchGiB >= 0 && p.ScratchGiB <= 1048576 && p.DiskOverheadGiB >= 1 && p.DiskOverheadGiB <= 1024, key+".disk", "ディスクまたは余裕が不足・範囲外です")
		check(p.MaxRunners >= 1 && p.MaxRunners <= 10000 && p.WarmIdle >= 0 && p.WarmIdle <= p.MaxRunners, key+".maxRunners", "台数が不正です")
		check(p.ExecutionMinutes >= 1 && p.ExecutionMinutes <= 7200, key+".executionMinutes", "実行上限は1〜7200分です")
		if p.NodeName != "" {
			check(nodes[p.NodeName], key+".nodeName", "Nodeが存在しません")
		}
		for k, v := range p.NodeSelector {
			check(ValidName(k) && ValidName(v), key+".nodeSelector", "選択条件が不正です")
		}
		if p.Enabled {
			found := false
			for _, n := range c.Nodes {
				if c.Eligible(p, n) && p.Charge().Fits(n.Budget) {
					found = true
				}
			}
			check(found, key+".capacity", "提供可能なNodeがありません")
		}
	}
	for _, n := range c.Nodes {
		seen := map[string]bool{}
		for _, p := range n.AllowedPools {
			check(pools[p] && !seen[p], "nodes."+n.Name+".allowedPools", "Poolが存在しないか重複しています")
			seen[p] = true
		}
	}
	reserved := map[string]Resources{}
	slots := map[string]int64{}
	names := map[string]bool{}
	for _, r := range c.Reservations {
		key := "reservations." + r.Name
		check(ValidName(r.Name) && !names[r.Name], key, "不正または重複した予約名です")
		names[r.Name] = true
		p, pok := c.Pool(r.Pool)
		n, nok := c.Node(r.Node)
		check(pok && nok && c.Eligible(p, n), key+".target", "予約対象が不適合です")
		check(r.Slots >= 1 && r.Slots <= 10000, key+".slots", "予約台数が不正です")
		if r.Slots >= 1 && r.Slots <= 10000 {
			reserved[r.Node] = reserved[r.Node].Add(p.Charge().Mul(r.Slots))
			slots[r.Pool] += r.Slots
		}
	}
	for k, r := range reserved {
		n, _ := c.Node(k)
		check(r.Fits(n.Budget), "nodes."+k+".reservations", "全予約を同時に提供できません")
	}
	for k, s := range slots {
		p, _ := c.Pool(k)
		check(s <= p.MaxRunners, "pools."+k+".reservations", "予約台数がPool上限を超えています")
	}
	if len(bad) > 0 {
		return Fail("INVALID_CONFIG", "設定を修正してください", bad)
	}
	return nil
}

// Decode is bounded and rejects ambiguous JSON before interpreting its fields.
func Decode(r io.Reader, dst any) error {
	b, e := io.ReadAll(io.LimitReader(r, MaxJSON+1))
	if e != nil {
		return e
	}
	if len(b) > MaxJSON || !utf8.Valid(b) {
		return Fail("INVALID_JSON", "UTF-8・1MiB以下で指定してください", nil)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e = unique(d, 0); e != nil {
		return Fail("INVALID_JSON", "JSONの構文または重複キーを確認してください", nil)
	}
	if _, e = d.Token(); e != io.EOF {
		return Fail("INVALID_JSON", "JSONの後ろに余分な値があります", nil)
	}
	if e = exactFields(b, dst); e != nil {
		return e
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(dst); e != nil {
		return Fail("INVALID_JSON", "JSONの型または項目を確認してください", nil)
	}
	return nil
}
func unique(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting limit")
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	x, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch x {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return errors.New("duplicate JSON key")
			}
			seen[s] = true
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
		t, e = d.Token()
		if e != nil || t != json.Delim('}') {
			return errors.New("unclosed object")
		}
	case '[':
		for d.More() {
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
		t, e = d.Token()
		if e != nil || t != json.Delim(']') {
			return errors.New("unclosed array")
		}
	default:
		return errors.New("unexpected delimiter")
	}
	return nil
}

type Instance struct {
	ID          string    `json:"id"`
	RequestID   string    `json:"requestID"`
	Node        string    `json:"node"`
	Pool        Pool      `json:"pool"`
	Image       Image     `json:"image"`
	State       string    `json:"state"`
	Reservation string    `json:"reservation,omitempty"`
	Held        Resources `json:"held"`
	RunnerID    int64     `json:"runnerID"`
	Result      string    `json:"result,omitempty"`
	Created     time.Time `json:"created"`
	Deadline    time.Time `json:"deadline"`
	Updated     time.Time `json:"updated"`
	JITReady    bool      `json:"jitReady"`
	// Task links an instance created for an agent task. JITReady then means
	// that the encrypted task payload, not a GitHub JIT configuration, is ready.
	Task string `json:"task,omitempty"`
}

func (a Instance) Name() string { return "rl-" + a.ID }
func (a Instance) Active() bool {
	return a.State != "Deleted" && a.State != "Deleting" && a.State != "Finishing"
}

type Observation struct {
	Node        string     `json:"node"`
	BootID      string     `json:"bootID"`
	Sequence    int64      `json:"sequence"`
	Seen        time.Time  `json:"seen"`
	Ready       bool       `json:"ready"`
	Drained     bool       `json:"drained"`
	Isolation   bool       `json:"isolation"`
	Ceiling     Resources  `json:"ceiling"`
	FreeDiskGiB int64      `json:"freeDiskGiB"`
	Digests     []string   `json:"digests"`
	Instances   []VMReport `json:"instances"`
	Error       string     `json:"error,omitempty"`
}
type VMReport struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}
type Command struct {
	Instance Instance     `json:"instance"`
	Action   string       `json:"action"`
	JIT      string       `json:"jit,omitempty"`
	Task     *TaskPayload `json:"task,omitempty"`
}
type SyncResponse struct {
	Commands        []Command `json:"commands"`
	IntervalSeconds int       `json:"intervalSeconds"`
	Images          []Image   `json:"images"`
}
type Demand struct {
	Pool    string    `json:"pool"`
	Desired int64     `json:"desired"`
	Seen    time.Time `json:"seen"`
	Blocked bool      `json:"blocked"`
}

func Contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func ValidateInstance(a Instance) error {
	p := a.Pool
	if p.VCPU < 1 || p.VCPU > 65536 || p.MemoryMiB < 512 || p.MemoryMiB > 1073741824 || p.OverheadMiB < 512 || p.OverheadMiB > 1048576 || p.RootGiB < 1 || p.RootGiB > 1048576 || p.ScratchGiB < 0 || p.ScratchGiB > 1048576 || p.DiskOverheadGiB < 1 || p.DiskOverheadGiB > 1024 {
		return errors.New("VM component size outside safe bounds")
	}

	if a.Task != "" && !ValidID(a.Task) {
		return fmt.Errorf("invalid task link")
	}
	if !ValidID(a.ID) || !ValidName(a.Node) || !ValidName(a.Pool.Name) || !digestPattern.MatchString(a.Image.Digest) || !a.Pool.Charge().Valid() || a.Deadline.IsZero() {
		return fmt.Errorf("invalid instance identity or resources")
	}
	return nil
}
