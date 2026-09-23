// Package tasks is the client side of agent tasks: the task-client identity,
// the mTLS connection to the Controller, agent profiles and the collection of
// locally stored agent credentials. It runs on the machine where the user's
// own coding assistant runs (Claude Code, Codex, ChatGPT desktop, ...).
package tasks

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/discovery"
)

// ClientConfig is the non-secret, user-owned client state. Allow lists the
// agents (and "github") whose local credentials may leave this machine.
type ClientConfig struct {
	Name         string   `json:"name"`
	Cluster      string   `json:"cluster,omitempty"`
	Controller   string   `json:"controller,omitempty"`
	Allow        []string `json:"allow"`
	DefaultPool  string   `json:"defaultPool,omitempty"`
	DefaultAgent string   `json:"defaultAgent,omitempty"`
	// Pinned records the digest of each allowed non-built-in profile. A
	// changed agents.json entry must be allowed again before it can run.
	Pinned map[string]string `json:"pinned,omitempty"`
}

// Bundle carries only public material from the Controller administrator back
// to the client: the pinned CA and the client certificate.
type Bundle struct {
	Name        string `json:"name"`
	Cluster     string `json:"cluster"`
	Controller  string `json:"controller"`
	CA          []byte `json:"ca"`
	Fingerprint string `json:"fingerprint"`
	Certificate []byte `json:"certificate"`
}

// ControllerOrigin validates the Controller URL as a bare https origin.
func (b Bundle) ControllerOrigin() (*url.URL, error) {
	u, e := url.Parse(b.Controller)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, core.Fail("CONTROLLER_URL", "Controllerは https://host:port の形式で指定してください", b.Controller)
	}
	return u, nil
}

// GitHubCredential is the allow-list entry that permits sending a GitHub token.
const GitHubCredential = "github"

// DefaultDir is $RUNNERLOOM_CLIENT_DIR, else $XDG_CONFIG_HOME/runnerloom/client,
// else ~/.config/runnerloom/client.
func DefaultDir() string {
	if d := os.Getenv("RUNNERLOOM_CLIENT_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "runnerloom", "client")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "runnerloom", "client")
}

// ResolveDir resolves symbolic links in the parents of dir once (for example a
// home reached through /home -> /data/home, or a stow-managed ~/.config), so
// the owner-only checks then apply to the real directory.
func ResolveDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", core.Fail("CLIENT_DIR", "クライアント設定ディレクトリは絶対パスで指定してください", dir)
	}
	parent := filepath.Dir(filepath.Clean(dir))
	for p := parent; ; p = filepath.Dir(p) {
		if _, e := os.Lstat(p); e == nil {
			real, e := filepath.EvalSymlinks(p)
			if e != nil {
				return "", e
			}
			rest, _ := filepath.Rel(p, parent)
			return filepath.Join(real, rest, filepath.Base(dir)), nil
		}
		if p == filepath.Dir(p) {
			return dir, nil
		}
	}
}

func LoadConfig(dir string) (ClientConfig, error) {
	var c ClientConfig
	b, e := core.ReadSecret(filepath.Join(dir, "client.json"))
	if os.IsNotExist(e) {
		return c, core.Fail("CLIENT_NOT_CONFIGURED", "このPCはまだタスククライアントとして登録されていません。runnerloom client init から始めてください", dir)
	}
	if e != nil {
		return c, e
	}
	if e = core.Decode(bytes.NewReader(b), &c); e != nil {
		return c, e
	}
	if !core.ValidName(c.Name) {
		return c, errors.New("invalid client name in client.json")
	}
	return c, nil
}

func SaveConfig(dir string, c ClientConfig) error {
	if c.Allow == nil {
		c.Allow = []string{}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return core.WritePrivate(filepath.Join(dir, "client.json"), append(b, '\n'))
}

// Init creates the client key and CSR. The key never leaves this machine; the
// CSR is public and is taken to the Controller administrator.
func Init(dir, name string) (csrPath, csrHash string, err error) {
	if !core.ValidName(name) {
		return "", "", core.Fail("CLIENT_INPUT", "クライアント名は英小文字・数字・ハイフンです", nil)
	}
	if e := core.PrivateDir(dir); e != nil {
		return "", "", e
	}
	keyPath := filepath.Join(dir, "client-key.pem")
	csrPath = filepath.Join(dir, "client.csr")
	if _, e := os.Lstat(keyPath); e == nil {
		return "", "", core.Fail("CLIENT_EXISTS", "このディレクトリには既にクライアント鍵があります。置き換えません", dir)
	} else if !os.IsNotExist(e) {
		return "", "", e
	}
	key, csr, e := core.NewKeyCSR()
	if e != nil {
		return "", "", e
	}
	if e = core.WritePrivate(keyPath, key); e != nil {
		return "", "", e
	}
	if e = core.WritePrivate(csrPath, csr); e != nil {
		return "", "", e
	}
	if e = SaveConfig(dir, ClientConfig{Name: name, Allow: []string{}}); e != nil {
		return "", "", e
	}
	parsed, e := core.ParseCSR(csr)
	if e != nil {
		return "", "", e
	}
	return csrPath, core.Hash(parsed.Raw), nil
}

func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	p, rest := pem.Decode(certPEM)
	if p == nil || p.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(p.Bytes)
}

// NormalizeFingerprint accepts "sha256:" prefixes and colon-separated hex.
func NormalizeFingerprint(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "sha256:")
	return strings.ReplaceAll(v, ":", "")
}

// Install verifies an approved bundle against the local key and pins the CA.
// caFingerprint must come from the administrator out of band (the approve
// output): the bundle is not secret, but a swapped bundle would otherwise pin
// an attacker's CA and send every later task there.
func Install(dir string, b Bundle, caFingerprint string, replace bool) (ClientConfig, error) {
	c, e := LoadConfig(dir)
	if e != nil {
		return c, e
	}
	if NormalizeFingerprint(caFingerprint) != NormalizeFingerprint(b.Fingerprint) || NormalizeFingerprint(caFingerprint) == "" {
		return c, core.Fail("BUNDLE_MISMATCH", "CAの指紋が、Controllerで表示された値と一致しません", nil)
	}
	if old, e := core.ReadSecret(filepath.Join(dir, "ca.sha256")); e == nil && NormalizeFingerprint(string(old)) != NormalizeFingerprint(b.Fingerprint) && !replace {
		return c, core.Fail("CLIENT_EXISTS", "別のControllerのCAが既に登録されています。置き換えるなら --replace を指定してください", nil)
	} else if e != nil && !os.IsNotExist(e) {
		return c, e
	}
	if b.Name != c.Name {
		return c, core.Fail("BUNDLE_MISMATCH", "承認されたクライアント名がこのPCの名前と違います", b.Name)
	}
	u, e := b.ControllerOrigin()
	if e != nil || !core.ValidName(b.Cluster) {
		return c, core.Fail("BUNDLE_MISMATCH", "ControllerのURLまたはCluster名が不正です", nil)
	}
	keyPEM, e := core.ReadSecret(filepath.Join(dir, "client-key.pem"))
	if e != nil {
		return c, e
	}
	pair, e := tls.X509KeyPair(b.Certificate, keyPEM)
	if e != nil {
		return c, core.Fail("BUNDLE_MISMATCH", "証明書がこのPCの鍵に対応していません", nil)
	}
	if _, e = core.ClientTLS(b.CA, b.Fingerprint, u.Hostname(), &pair); e != nil {
		return c, core.Fail("BUNDLE_MISMATCH", "CAの指紋が一致しません", nil)
	}
	leaf, e := parseLeaf(b.Certificate)
	if e != nil {
		return c, e
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(b.CA)
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e != nil {
		return c, core.Fail("BUNDLE_MISMATCH", "証明書がCAで検証できません", nil)
	}
	if name, e := core.ClientIdentity(leaf, b.Cluster); e != nil || name != c.Name {
		return c, core.Fail("BUNDLE_MISMATCH", "証明書のクライアントIDが一致しません", nil)
	}
	for name, v := range map[string][]byte{"ca.pem": b.CA, "ca.sha256": []byte(b.Fingerprint), "client.pem": b.Certificate} {
		if e = core.WritePrivate(filepath.Join(dir, name), v); e != nil {
			return c, e
		}
	}
	c.Cluster, c.Controller = b.Cluster, b.Controller
	return c, SaveConfig(dir, c)
}

// Client talks to the Controller's task API with the client certificate.
type Client struct {
	Dir     string
	Config  ClientConfig
	mu      sync.Mutex
	http    *http.Client
	checked time.Time
}

func Open(dir string) (*Client, error) {
	c, e := LoadConfig(dir)
	if e != nil {
		return nil, e
	}
	if c.Controller == "" || c.Cluster == "" {
		return nil, core.Fail("CLIENT_NOT_APPROVED", "Controllerで承認した証明書をまだ取り込んでいません。runnerloom client install を実行してください", nil)
	}
	cl := &Client{Dir: dir, Config: c}
	if e = cl.reload(); e != nil {
		return nil, e
	}
	return cl, nil
}

func (c *Client) reload() error {
	ca, e := core.ReadSecret(filepath.Join(c.Dir, "ca.pem"))
	if e != nil {
		return e
	}
	pin, e := core.ReadSecret(filepath.Join(c.Dir, "ca.sha256"))
	if e != nil {
		return e
	}
	certPEM, e := core.ReadSecret(filepath.Join(c.Dir, "client.pem"))
	if e != nil {
		return e
	}
	keyPEM, e := core.ReadSecret(filepath.Join(c.Dir, "client-key.pem"))
	if e != nil {
		return e
	}
	pair, e := tls.X509KeyPair(certPEM, keyPEM)
	if e != nil {
		return e
	}
	u, e := url.Parse(c.Config.Controller)
	if e != nil {
		return e
	}
	cfg, e := core.ClientTLS(ca, strings.TrimSpace(string(pin)), u.Hostname(), &pair)
	if e != nil {
		return e
	}
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	c.http = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("controller redirects are forbidden") }, Transport: &http.Transport{TLSClientConfig: cfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		// Linux resolves .local through Avahi here; macOS resolves it natively.
		if runtime.GOOS == "linux" && strings.HasSuffix(strings.ToLower(host), ".local") {
			ip, e := discovery.ResolveLocal(ctx, host)
			if e != nil {
				return nil, e
			}
			address = net.JoinHostPort(ip, port)
		}
		d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, address)
	}}}
	return nil
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	hc := c.http
	c.mu.Unlock()
	var reader *bytes.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		if len(b) > core.MaxJSON {
			return core.Fail("TASK_TOO_LARGE", "送信内容が1MiBを超えています", nil)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	r, e := http.NewRequestWithContext(ctx, method, c.Config.Controller+path, reader)
	if e != nil {
		return e
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, e := hc.Do(r)
	if e != nil {
		return core.Fail("CONTROLLER_UNREACHABLE", "Controllerに接続できません: "+e.Error(), c.Config.Controller)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var failure struct {
			Error *core.Error `json:"error"`
		}
		if e = core.Decode(resp.Body, &failure); e == nil && failure.Error != nil {
			return failure.Error
		}
		return fmt.Errorf("controller returned HTTP %d", resp.StatusCode)
	}
	return core.Decode(resp.Body, out)
}

// renew refreshes the client certificate with the same key well before expiry.
func (c *Client) renew(ctx context.Context) error {
	c.mu.Lock()
	if time.Since(c.checked) < time.Hour {
		c.mu.Unlock()
		return nil
	}
	c.checked = time.Now()
	c.mu.Unlock()
	// Several processes of one client (MCP servers, CLI) may renew at the same
	// time; one renews, the others then see the fresh certificate.
	lock, e := os.OpenFile(filepath.Join(c.Dir, "renew.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	certPEM, e := core.ReadSecret(filepath.Join(c.Dir, "client.pem"))
	if e != nil {
		return e
	}
	leaf, e := parseLeaf(certPEM)
	if e != nil {
		return e
	}
	if time.Until(leaf.NotAfter) > 30*24*time.Hour {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.reload() // another process may have renewed it
	}
	keyPEM, e := core.ReadSecret(filepath.Join(c.Dir, "client-key.pem"))
	if e != nil {
		return e
	}
	p, _ := pem.Decode(keyPEM)
	if p == nil {
		return errors.New("invalid client key")
	}
	key, e := x509.ParseECPrivateKey(p.Bytes)
	if e != nil {
		return e
	}
	var signer *ecdsa.PrivateKey = key
	der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, signer)
	if e != nil {
		return e
	}
	var reply struct {
		Certificate []byte `json:"certificate"`
	}
	q := struct {
		CSR []byte `json:"csr"`
	}{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})}
	if e = c.do(ctx, http.MethodPost, "/v1/client/renew", q, &reply); e != nil {
		return e
	}
	fresh, e := parseLeaf(reply.Certificate)
	if e != nil {
		return e
	}
	if name, e := core.ClientIdentity(fresh, c.Config.Cluster); e != nil || name != c.Config.Name {
		return errors.New("renewal returned another client identity")
	}
	if e = core.WritePrivate(filepath.Join(c.Dir, "client.pem"), reply.Certificate); e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reload()
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	if e := c.renew(ctx); e != nil {
		var fault *core.Error
		if errors.As(e, &fault) && fault.Code == "CLIENT_UNAUTHORIZED" {
			return e
		}
		// A failed early renewal is retried later; the current certificate
		// remains valid for weeks.
	}
	e := c.do(ctx, method, path, body, out)
	var fault *core.Error
	if errors.As(e, &fault) && fault.Code == "CLIENT_UNAUTHORIZED" {
		// Another process may have renewed the certificate on disk.
		c.mu.Lock()
		reloadErr := c.reload()
		c.mu.Unlock()
		if reloadErr == nil {
			return c.do(ctx, method, path, body, out)
		}
	}
	return e
}

func (c *Client) Pools(ctx context.Context) ([]core.TaskPool, error) {
	var v struct {
		Pools []core.TaskPool `json:"pools"`
	}
	return v.Pools, c.call(ctx, http.MethodGet, "/v1/task-pools", nil, &v)
}
func (c *Client) Submit(ctx context.Context, spec core.TaskSpec) (core.Task, error) {
	var v core.Task
	return v, c.call(ctx, http.MethodPost, "/v1/tasks", spec, &v)
}
func (c *Client) Task(ctx context.Context, id string) (core.Task, error) {
	var v core.Task
	if !core.ValidID(id) {
		return v, core.Fail("TASK_NOT_FOUND", "タスクIDが不正です", nil)
	}
	return v, c.call(ctx, http.MethodGet, "/v1/tasks/"+id, nil, &v)
}
func (c *Client) Tasks(ctx context.Context) ([]core.Task, error) {
	var v struct {
		Tasks []core.Task `json:"tasks"`
	}
	return v.Tasks, c.call(ctx, http.MethodGet, "/v1/tasks", nil, &v)
}
func (c *Client) Cancel(ctx context.Context, id string) (core.Task, error) {
	var v core.Task
	if !core.ValidID(id) {
		return v, core.Fail("TASK_NOT_FOUND", "タスクIDが不正です", nil)
	}
	return v, c.call(ctx, http.MethodPost, "/v1/tasks/"+id+"/cancel", struct{}{}, &v)
}
