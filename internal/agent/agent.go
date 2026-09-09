// Package agent implements the outbound-only node worker. Administrative
// credentials never travel to the node; only one-job JIT configurations do.
package agent

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
	"github.com/MOVEI144/RunnerLoom/internal/discovery"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
)

type Config struct {
	Node        string         `json:"node"`
	Cluster     string         `json:"cluster"`
	Controller  string         `json:"controller"`
	StateDir    string         `json:"stateDir"`
	DiskDir     string         `json:"diskDir"`
	NetworkCIDR string         `json:"networkCIDR"`
	Ceiling     core.Resources `json:"ceiling"`
	CacheGiB    int64          `json:"cacheGiB"`
	QEMUUser    string         `json:"qemuUser"`
}

func (c Config) Validate() error {
	u, e := url.Parse(c.Controller)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("controller must be an https origin")
	}
	if !core.ValidName(c.Node) || !core.ValidName(c.Cluster) || !c.Ceiling.Valid() || c.CacheGiB < 1 || c.CacheGiB > 1048576 {
		return errors.New("invalid node configuration")
	}
	for _, p := range []string{c.StateDir, c.DiskDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return errors.New("node paths must be normalized absolute paths")
		}
	}
	if c.StateDir == c.DiskDir || strings.HasPrefix(c.DiskDir, c.StateDir+"/") {
		return errors.New("VM disk directory must be outside the private agent state directory")
	}
	return nil
}
func SaveConfig(path string, c Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return core.WritePrivate(path, b)
}
func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := core.ReadSecret(path)
	if e != nil {
		return c, e
	}
	if e = core.Decode(bytes.NewReader(b), &c); e != nil {
		return c, e
	}
	return c, c.Validate()
}

type Client struct {
	URL  string
	HTTP *http.Client
}

func NewClient(origin, state string) (*Client, error) {
	ca, e := core.ReadSecret(filepath.Join(state, "ca.pem"))
	if e != nil {
		return nil, e
	}
	pin, e := core.ReadSecret(filepath.Join(state, "ca.sha256"))
	if e != nil {
		return nil, e
	}
	certPEM, e := core.ReadSecret(filepath.Join(state, "node.pem"))
	if e != nil {
		return nil, e
	}
	keyPEM, e := core.ReadSecret(filepath.Join(state, "node-key.pem"))
	if e != nil {
		return nil, e
	}
	certificate, e := tls.X509KeyPair(certPEM, keyPEM)
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(origin)
	if e != nil {
		return nil, e
	}
	cfg, e := core.ClientTLS(ca, strings.TrimSpace(string(pin)), u.Hostname(), &certificate)
	if e != nil {
		return nil, e
	}
	return client(origin, cfg), nil
}
func client(origin string, cfg *tls.Config) *Client {
	return &Client{URL: origin, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		if strings.HasSuffix(strings.ToLower(host), ".local") {
			ip, e := discovery.ResolveLocal(ctx, host)
			if e != nil {
				return nil, e
			}
			address = net.JoinHostPort(ip, port)
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		return dialer.DialContext(ctx, network, address)
	}, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("controller redirects are forbidden") }}}
}
func (c *Client) Post(ctx context.Context, path string, q, v any) error {
	b, e := json.Marshal(q)
	if e != nil {
		return e
	}
	if len(b) > core.MaxJSON {
		return errors.New("node message exceeds limit")
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return e
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
	return core.Decode(resp.Body, v)
}
func Enroll(ctx context.Context, invite core.Invitation, c Config) (core.Enrollment, error) {
	if e := c.Validate(); e != nil {
		return core.Enrollment{}, e
	}
	if c.Controller != invite.URL {
		return core.Enrollment{}, errors.New("invitation endpoint differs from node configuration")
	}
	if time.Now().After(invite.Expires) {
		return core.Enrollment{}, errors.New("invitation expired")
	}
	if e := core.PrivateDir(c.StateDir); e != nil {
		return core.Enrollment{}, e
	}
	for _, name := range []string{"node.pem", "ca.pem"} {
		if _, e := os.Lstat(filepath.Join(c.StateDir, name)); e == nil {
			return core.Enrollment{}, errors.New("already enrolled; never replace an existing node identity")
		} else if !os.IsNotExist(e) {
			return core.Enrollment{}, e
		}
	}
	keyPath := filepath.Join(c.StateDir, "node-key.pem")
	csrPath := filepath.Join(c.StateDir, "node.csr")
	csr, e := core.ReadSecret(csrPath)
	if os.IsNotExist(e) {
		if _, e = os.Lstat(keyPath); !os.IsNotExist(e) {
			return core.Enrollment{}, errors.New("partial node identity requires explicit recovery")
		}
		key, request, e := core.NewKeyCSR()
		if e != nil {
			return core.Enrollment{}, e
		}
		if e = core.WritePrivate(keyPath, key); e != nil {
			return core.Enrollment{}, e
		}
		if e = core.WritePrivate(csrPath, request); e != nil {
			return core.Enrollment{}, e
		}
		csr = request
	} else if e != nil {
		return core.Enrollment{}, e
	}
	u, e := url.Parse(invite.URL)
	if e != nil {
		return core.Enrollment{}, e
	}
	cfg, e := core.ClientTLS(invite.CA, invite.Fingerprint, u.Hostname(), nil)
	if e != nil {
		return core.Enrollment{}, e
	}
	cl := client(invite.URL, cfg)
	defer cl.HTTP.CloseIdleConnections()
	q := core.JoinRequest{ID: invite.ID, Secret: invite.Secret, Name: c.Node, CSR: csr, Ceiling: c.Ceiling}
	var out core.Enrollment
	if e = cl.Post(ctx, "/v1/enroll", q, &out); e != nil {
		return out, e
	}
	if out.Status == "Approved" {
		key, e := core.ReadSecret(keyPath)
		if e != nil {
			return out, e
		}
		cert, e := tls.X509KeyPair(out.Certificate, key)
		if e != nil {
			return out, e
		}
		leaf, e := x509.ParseCertificate(cert.Certificate[0])
		if e != nil {
			return out, e
		}
		identity, identityErr := core.NodeIdentity(leaf, c.Cluster)
		if identityErr != nil || identity != c.Node {
			return out, errors.New("returned node certificate identity mismatch")
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(invite.CA)
		if _, e = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e != nil {
			return out, e
		}
		if e = core.WritePrivate(filepath.Join(c.StateDir, "ca.sha256"), []byte(invite.Fingerprint)); e != nil {
			return out, e
		}
		if e = core.WritePrivate(filepath.Join(c.StateDir, "ca.pem"), invite.CA); e != nil {
			return out, e
		}
		if e = core.WritePrivate(filepath.Join(c.StateDir, "node.pem"), out.Certificate); e != nil {
			return out, e
		}
		if e = SaveConfig(filepath.Join(c.StateDir, "agent.json"), c); e != nil {
			return out, e
		}
	}
	return out, nil
}

type Agent struct {
	FreeDisk func(string) (int64, error)
	Config   Config
	Provider host.Provider
	Images   *host.Images
	Client   *Client
	Log      *slog.Logger
	boot     string
	sequence int64
	catalog  []core.Image
}

func New(c Config) (*Agent, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	cl, e := NewClient(c.Controller, c.StateDir)
	if e != nil {
		return nil, e
	}
	ex := host.SystemExecutor{}
	images := &host.Images{Dir: filepath.Join(c.StateDir, "images"), LimitGiB: c.CacheGiB, Exec: ex}
	network := host.Network{Cluster: c.Cluster, CIDR: c.NetworkCIDR, Dir: filepath.Join(c.StateDir, "network"), Exec: ex}
	p := &host.Libvirt{StateDir: c.StateDir, DiskDir: c.DiskDir, Node: c.Node, Cluster: c.Cluster, Ceiling: c.Ceiling, Network: network, Images: images, Exec: ex, QEMUUser: c.QEMUUser}
	a := &Agent{Config: c, Provider: p, Images: images, Client: cl, Log: slog.Default(), boot: core.ID()}
	seq, e := core.ReadSecret(filepath.Join(c.StateDir, "sequence"))
	if e == nil {
		a.sequence, e = strconv.ParseInt(string(seq), 10, 64)
		if e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if b, e := core.ReadSecret(filepath.Join(c.StateDir, "catalog.json")); e == nil {
		if e = core.Decode(bytes.NewReader(b), &a.catalog); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return a, nil
}
func (a *Agent) renew(ctx context.Context) error {
	b, e := core.ReadSecret(filepath.Join(a.Config.StateDir, "node.pem"))
	if e != nil {
		return e
	}
	p, _ := pem.Decode(b)
	if p == nil {
		return errors.New("invalid node certificate")
	}
	cert, e := x509.ParseCertificate(p.Bytes)
	if e != nil {
		return e
	}
	if time.Until(cert.NotAfter) > 7*24*time.Hour {
		return nil
	}
	b, e = core.ReadSecret(filepath.Join(a.Config.StateDir, "node-key.pem"))
	if e != nil {
		return e
	}
	p, _ = pem.Decode(b)
	if p == nil {
		return errors.New("invalid node key")
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
	if e = a.Client.Post(ctx, "/v1/renew", q, &reply); e != nil {
		return e
	}
	pair, e := tls.X509KeyPair(reply.Certificate, b)
	if e != nil {
		return e
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return e
	}
	name, e := core.NodeIdentity(leaf, a.Config.Cluster)
	if e != nil || name != a.Config.Node {
		return errors.New("renewal returned wrong node identity")
	}
	if e = core.WritePrivate(filepath.Join(a.Config.StateDir, "node.pem"), reply.Certificate); e != nil {
		return e
	}
	fresh, e := NewClient(a.Config.Controller, a.Config.StateDir)
	if e != nil {
		return e
	}
	a.Client.HTTP.CloseIdleConnections()
	a.Client = fresh
	return nil
}
func (a *Agent) download(ctx context.Context, im core.Image) error {
	if e := a.Images.Verify(ctx, im.Digest); e == nil {
		return nil
	}
	// Separate client for bounded streaming; credential/TLS/redirect policy retained.
	httpClient := *a.Client.HTTP
	httpClient.Timeout = 30 * time.Minute
	r, e := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.Controller+"/v1/images/"+strings.TrimPrefix(im.Digest, "sha256:"), nil)
	if e != nil {
		return e
	}
	resp, e := httpClient.Do(r)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("image source returned HTTP %d", resp.StatusCode)
	}
	_, e = a.Images.Import(ctx, resp.Body, im.Digest)
	return e
}
func (a *Agent) Step(ctx context.Context) error {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	reports, e := a.Provider.Inventory(ctx)
	if e != nil {
		return e
	}
	ready := a.Provider.Ready(ctx) == nil
	digests, e := a.Images.Available(ctx, a.catalog)
	if e != nil {
		ready = false
		digests = []string{}
	}
	freeFn := host.FreeGiB
	if a.FreeDisk != nil {
		freeFn = a.FreeDisk
	}
	free, e := freeFn(a.Config.DiskDir)
	if e != nil {
		return e
	}
	a.sequence++
	if e = core.WritePrivate(filepath.Join(a.Config.StateDir, "sequence"), []byte(strconv.FormatInt(a.sequence, 10))); e != nil {
		return e
	}
	o := core.Observation{Node: a.Config.Node, BootID: a.boot, Sequence: a.sequence, Ready: ready, Isolation: ready, Ceiling: a.Config.Ceiling, FreeDiskGiB: free, Digests: digests, Instances: reports}
	var reply struct {
		Commands        []core.Command `json:"commands"`
		IntervalSeconds int            `json:"intervalSeconds"`
		Images          []core.Image   `json:"images"`
	}
	if e = a.Client.Post(ctx, "/v1/sync", o, &reply); e != nil {
		return e
	}
	if len(reply.Images) > 1024 || len(reply.Commands) > 8 {
		return errors.New("controller response exceeds policy")
	}
	if core.Fingerprint(reply.Images) != core.Fingerprint(a.catalog) {
		b, _ := json.Marshal(reply.Images)
		if e = core.WritePrivate(filepath.Join(a.Config.StateDir, "catalog.json"), b); e != nil {
			return e
		}
		a.catalog = reply.Images
	}
	for _, cmd := range reply.Commands {
		if cmd.Instance.Node != a.Config.Node || !cmd.Instance.Pool.Charge().Fits(a.Config.Ceiling) {
			return errors.New("controller command exceeds node policy")
		}
		switch cmd.Action {
		case "ensure":
			e = a.Provider.Ensure(ctx, cmd.Instance, cmd.JIT)
		case "stop":
			if preparer, ok := a.Provider.(interface {
				EnsureStopIntent(context.Context, core.Instance) error
			}); ok {
				e = preparer.EnsureStopIntent(ctx, cmd.Instance)
				if e != nil {
					break
				}
			}
			e = a.Provider.Stop(ctx, cmd.Instance.ID)
		case "delete":
			e = a.Provider.Delete(ctx, cmd.Instance.ID)
		default:
			return errors.New("unknown controller operation")
		}
		if e != nil {
			a.Log.Warn("node operation not completed", "instance", cmd.Instance.ID, "action", cmd.Action, "error", e.Error())
		}
	}
	// Downloads occur only when requested by the authenticated controller and are
	// SHA checked before publication. No guest can select an arbitrary download URL.
	for _, im := range a.catalog {
		if e = a.Images.Verify(ctx, im.Digest); e == nil {
			continue
		}
		if e = a.download(ctx, im); e != nil {
			a.Log.Warn("image download not completed", "image", im.Name, "error", e.Error())
		}
		break
	}
	return nil
}
func (a *Agent) Run(ctx context.Context) error {
	if init, ok := a.Provider.(interface{ Init(context.Context) error }); ok {
		if e := init.Init(ctx); e != nil {
			return e
		}
	}
	defer a.Client.HTTP.CloseIdleConnections()
	if p, ok := a.Provider.(io.Closer); ok {
		defer p.Close()
	}
	delay := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		e := a.Step(ctx)
		if e != nil {
			a.Log.Warn("node synchronization failed", "error", e.Error())
			delay = min(30*time.Second, delay*2)
		} else {
			delay = 5 * time.Second
			if e = a.renew(ctx); e != nil {
				a.Log.Warn("certificate renewal deferred", "error", e.Error())
			}
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
