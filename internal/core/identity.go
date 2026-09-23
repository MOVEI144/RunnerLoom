package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ioCopyBounded(w io.Writer, r io.Reader, n int64) (int64, error) {
	written, e := io.Copy(w, io.LimitReader(r, n+1))
	if written > n {
		return written, errors.New("input exceeds limit")
	}
	return written, e
}

type CA struct {
	Certificate *x509.Certificate
	Key         *ecdsa.PrivateKey
	PEM         []byte
}

func serial() *big.Int {
	n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		panic(e)
	}
	return n
}
func LoadCA(dir string) (CA, error) {
	b, e := ReadSecret(filepath.Join(dir, "ca.pem"))
	if e != nil {
		return CA{}, e
	}
	p, rest := pem.Decode(b)
	if p == nil || len(rest) > 0 {
		return CA{}, errors.New("invalid CA PEM")
	}
	cert, e := x509.ParseCertificate(p.Bytes)
	if e != nil {
		return CA{}, e
	}
	b, e = ReadSecret(filepath.Join(dir, "ca-key.pem"))
	if e != nil {
		return CA{}, e
	}
	k, rest := pem.Decode(b)
	if k == nil || len(rest) > 0 {
		return CA{}, errors.New("invalid CA private key")
	}
	key, e := x509.ParseECPrivateKey(k.Bytes)
	if e != nil {
		return CA{}, e
	}
	pub, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return CA{}, e
	}
	expected, e := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if e != nil {
		return CA{}, e
	}
	if !cert.IsCA || cert.CheckSignatureFrom(cert) != nil || Hash(pub) != Hash(expected) {
		return CA{}, errors.New("CA key or trust mismatch")
	}
	return CA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})}, nil
}
func InitCA(dir, cluster string) (CA, error) {
	if e := PrivateDir(dir); e != nil {
		return CA{}, e
	}
	if _, e := os.Lstat(filepath.Join(dir, "ca.pem")); e == nil {
		return LoadCA(dir)
	} else if !os.IsNotExist(e) {
		return CA{}, e
	}
	if _, e := os.Lstat(filepath.Join(dir, "ca-key.pem")); !os.IsNotExist(e) {
		return CA{}, errors.New("partial CA state: restore rather than regenerate")
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return CA{}, e
	}
	now := time.Now().UTC()
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RunnerLoom " + cluster}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if e != nil {
		return CA{}, e
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return CA{}, e
	}
	if e = WritePrivate(filepath.Join(dir, "ca-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})); e != nil {
		return CA{}, e
	}
	if e = WritePrivate(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); e != nil {
		return CA{}, e
	}
	return LoadCA(dir)
}
func NewKeyCSR() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}
func ParseCSR(b []byte) (*x509.CertificateRequest, error) {
	if len(b) > 16384 {
		return nil, errors.New("CSR too large")
	}
	p, rest := pem.Decode(b)
	if p == nil || p.Type != "CERTIFICATE REQUEST" || len(rest) > 0 {
		return nil, errors.New("invalid CSR")
	}
	c, e := x509.ParseCertificateRequest(p.Bytes)
	if e != nil {
		return nil, e
	}
	if e = c.CheckSignature(); e != nil {
		return nil, e
	}
	k, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("P-256 node key required")
	}
	return c, nil
}
func (ca CA) Sign(csr []byte, cluster, node string) ([]byte, error) {
	return ca.sign(csr, cluster, "node", node, 30*24*time.Hour)
}

// ClientCertificateLifetime is longer than a Node's because a task client is
// usually a laptop that may stay offline for weeks; it still renews in-band.
const ClientCertificateLifetime = 90 * 24 * time.Hour

// SignClient issues a task-client identity. Its URI kind differs from a Node's,
// so a client certificate can never synchronize VMs and a Node certificate can
// never submit tasks.
func (ca CA) SignClient(csr []byte, cluster, name string) ([]byte, error) {
	return ca.sign(csr, cluster, "client", name, ClientCertificateLifetime)
}
func (ca CA) sign(csr []byte, cluster, kind, name string, lifetime time.Duration) ([]byte, error) {
	if !ValidName(cluster) || !ValidName(name) || (kind != "node" && kind != "client") {
		return nil, errors.New("invalid certificate identity")
	}
	req, e := ParseCSR(csr)
	if e != nil {
		return nil, e
	}
	now := time.Now().UTC()
	uri := &url.URL{Scheme: "spiffe", Host: "runnerloom", Path: "/cluster/" + cluster + "/" + kind + "/" + name}
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: name}, URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(lifetime), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	if tpl.NotAfter.After(ca.Certificate.NotAfter) {
		return nil, errors.New("CA renewal required")
	}
	der, e := x509.CreateCertificate(rand.Reader, tpl, ca.Certificate, req.PublicKey, ca.Key)
	if e != nil {
		return nil, e
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
func (ca CA) ServerCertificate(names []string) (tls.Certificate, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return tls.Certificate{}, e
	}
	now := time.Now().UTC()
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RunnerLoom controller"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(7 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, n)
		}
	}
	der, e := x509.CreateCertificate(rand.Reader, tpl, ca.Certificate, &key.PublicKey, ca.Key)
	if e != nil {
		return tls.Certificate{}, e
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return tls.Certificate{}, e
	}
	return tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}
func ClientTLS(caPEM []byte, pin, hostname string, certificate *tls.Certificate) (*tls.Config, error) {
	p, rest := pem.Decode(caPEM)
	if p == nil || len(rest) > 0 {
		return nil, errors.New("invalid CA")
	}
	ca, e := x509.ParseCertificate(p.Bytes)
	if e != nil {
		return nil, e
	}
	if !ca.IsCA || Hash(ca.Raw) != pin {
		return nil, errors.New("controller fingerprint mismatch")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: hostname}
	if certificate != nil {
		cfg.Certificates = []tls.Certificate{*certificate}
	}
	return cfg, nil
}
func NodeIdentity(cert *x509.Certificate, cluster string) (string, error) {
	return peerIdentity(cert, cluster, "node")
}

// ClientIdentity accepts only task-client certificates of this cluster.
func ClientIdentity(cert *x509.Certificate, cluster string) (string, error) {
	return peerIdentity(cert, cluster, "client")
}
func peerIdentity(cert *x509.Certificate, cluster, kind string) (string, error) {
	if cert != nil && (time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter)) {
		return "", errors.New(kind + " certificate is expired or not yet valid")
	}
	if cert == nil || cert.IsCA || len(cert.URIs) != 1 {
		return "", errors.New("invalid " + kind + " certificate")
	}
	u := cert.URIs[0]
	prefix := "/cluster/" + cluster + "/" + kind + "/"
	if u.Scheme != "spiffe" || u.Host != "runnerloom" || !strings.HasPrefix(u.Path, prefix) {
		return "", errors.New(kind + " belongs to a different cluster or role")
	}
	n := strings.TrimPrefix(u.Path, prefix)
	if !ValidName(n) {
		return "", errors.New("invalid " + kind + " name")
	}
	return n, nil
}

type Invitation struct {
	ID          string    `json:"id"`
	Secret      string    `json:"secret"`
	URL         string    `json:"url"`
	CA          []byte    `json:"ca"`
	Fingerprint string    `json:"fingerprint"`
	Expires     time.Time `json:"expires"`
}
type JoinRequest struct {
	ID      string    `json:"id"`
	Secret  string    `json:"secret"`
	Name    string    `json:"name"`
	CSR     []byte    `json:"csr"`
	Ceiling Resources `json:"ceiling"`
}
type Enrollment struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	CSRHash     string    `json:"csrHash"`
	Ceiling     Resources `json:"ceiling"`
	Status      string    `json:"status"`
	Certificate []byte    `json:"certificate,omitempty"`
}

func (s *Store) Invite(ctx context.Context, ca CA, endpoint string, ttl time.Duration) (Invitation, error) {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return Invitation{}, errors.New("advertised endpoint must be an https origin")
	}
	if ttl < time.Minute || ttl > time.Hour {
		return Invitation{}, errors.New("invitation lifetime must be 1..60 minutes")
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return Invitation{}, e
	}
	i := Invitation{ID: ID(), Secret: base64.RawURLEncoding.EncodeToString(b), URL: endpoint, CA: ca.PEM, Fingerprint: Hash(ca.Certificate.Raw), Expires: s.Now().UTC().Add(ttl)}
	_, e = s.DB.ExecContext(ctx, "INSERT INTO invites(id,hash,expires) VALUES(?,?,?)", i.ID, Hash([]byte(i.Secret)), i.Expires.Unix())
	return i, e
}
func (s *Store) Join(ctx context.Context, q JoinRequest) (out Enrollment, err error) {
	if !ValidName(q.Name) || !ValidID(q.ID) || len(q.Secret) > 128 || !q.Ceiling.Valid() {
		return out, Fail("ENROLLMENT_INPUT", "参加申請が不正です", nil)
	}
	csr, e := ParseCSR(q.CSR)
	if e != nil {
		return out, e
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var hash string
		var expires int64
		var used, revoked bool
		e := tx.QueryRowContext(ctx, "SELECT hash,expires,used,revoked FROM invites WHERE id=?", q.ID).Scan(&hash, &expires, &used, &revoked)
		if e != nil || revoked || s.Now().Unix() >= expires || subtle.ConstantTimeCompare([]byte(hash), []byte(Hash([]byte(q.Secret)))) != 1 {
			return Fail("INVITE_INVALID", "招待が無効または期限切れです", nil)
		}
		var b, cert, ceiling []byte
		var name, status, id string
		e = tx.QueryRowContext(ctx, "SELECT id,name,csr,ceiling,status,certificate FROM enrollments WHERE invite=?", q.ID).Scan(&id, &name, &b, &ceiling, &status, &cert)
		if e == nil {
			if Hash(b) != Hash(q.CSR) || name != q.Name {
				return Fail("INVITE_BOUND", "この招待は別の参加申請に使用されています", nil)
			}
			var r Resources
			if e = json.Unmarshal(ceiling, &r); e != nil {
				return e
			}
			if r != q.Ceiling {
				return Fail("INVITE_BOUND", "参加時の上限を変更できません", nil)
			}
			out = Enrollment{id, name, Hash(csr.Raw), r, status, cert}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if used {
			return Fail("INVITE_USED", "招待は使用済みです", nil)
		}
		out = Enrollment{ID: ID(), Name: q.Name, CSRHash: Hash(csr.Raw), Ceiling: q.Ceiling, Status: "PendingApproval"}
		ceiling, _ = json.Marshal(q.Ceiling)
		_, e = tx.ExecContext(ctx, "INSERT INTO enrollments(id,invite,name,csr,ceiling,status) VALUES(?,?,?,?,?,?)", out.ID, q.ID, q.Name, q.CSR, ceiling, out.Status)
		return e
	})
	return
}
func (s *Store) Pending(ctx context.Context) ([]Enrollment, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,name,csr,ceiling,status FROM enrollments WHERE status='PendingApproval' ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Enrollment{}
	for rows.Next() {
		var v Enrollment
		var csr, b []byte
		if e = rows.Scan(&v.ID, &v.Name, &csr, &b, &v.Status); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v.Ceiling); e != nil {
			return nil, e
		}
		parsed, e := ParseCSR(csr)
		if e != nil {
			return nil, e
		}
		v.CSRHash = Hash(parsed.Raw)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Approve(ctx context.Context, id string, ca CA) (out Enrollment, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var csr, b []byte
		var invite string
		e := tx.QueryRowContext(ctx, "SELECT id,name,csr,ceiling,status,invite,certificate FROM enrollments WHERE id=?", id).Scan(&out.ID, &out.Name, &csr, &b, &out.Status, &invite, &out.Certificate)
		if e != nil {
			return Fail("ENROLLMENT_NOT_FOUND", "参加申請がありません", nil)
		}
		if e = json.Unmarshal(b, &out.Ceiling); e != nil {
			return e
		}
		parsed, e := ParseCSR(csr)
		if e != nil {
			return e
		}
		out.CSRHash = Hash(parsed.Raw)
		if out.Status == "Approved" {
			return nil
		}
		var used, revoked bool
		var expires int64
		if e = tx.QueryRowContext(ctx, "SELECT used,revoked,expires FROM invites WHERE id=?", invite).Scan(&used, &revoked, &expires); e != nil {
			return e
		}
		if used || revoked || s.Now().Unix() >= expires {
			return Fail("INVITE_INVALID", "招待が無効になっています", nil)
		}
		c, rev, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		if rev == 0 {
			return Fail("NOT_CONFIGURED", "先にClusterを設定してください", nil)
		}
		var existing int
		e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE name=?", out.Name).Scan(&existing)
		if e != nil {
			return e
		}
		if existing > 0 {
			return Fail("NODE_NAME_EXISTS", "同名の登録済みNodeがあります。無断で置換しません", out.Name)
		}
		n, ok := c.Node(out.Name)
		if ok {
			if !n.Budget.Fits(out.Ceiling) {
				return Fail("LOCAL_CEILING", "登録済み予算がNodeの承認上限を超えています", nil)
			}
		} else {
			n = Node{Name: out.Name, Budget: out.Ceiling, LocalCeiling: out.Ceiling, AllowedPools: []string{}}
			for _, p := range c.Pools {
				if p.Charge().Fits(out.Ceiling) {
					n.AllowedPools = append(n.AllowedPools, p.Name)
				}
			}
			c.Nodes = append(c.Nodes, n)
		}
		if e = c.Validate(); e != nil {
			return e
		}
		out.Certificate, e = ca.Sign(csr, c.Name, out.Name)
		if e != nil {
			return e
		}
		out.Status = "Approved"
		_, e = tx.ExecContext(ctx, "INSERT INTO identities(name,certificate_hash) VALUES(?,?)", out.Name, Hash(out.Certificate))
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE enrollments SET status=?,certificate=? WHERE id=?", out.Status, out.Certificate, id)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE invites SET used=1 WHERE id=?", invite)
		if e != nil {
			return e
		}
		b, _ = json.Marshal(c)
		_, e = tx.ExecContext(ctx, "UPDATE settings SET revision=?,payload=? WHERE id=1", rev+1, b)
		return e
	})
	return
}
func (s *Store) Authorize(ctx context.Context, name string) error {
	return s.authorize(ctx, name, "")
}

func (s *Store) AuthorizePeer(ctx context.Context, name string, certPEM []byte) error {
	return s.authorize(ctx, name, Hash(certPEM))
}

func (s *Store) authorize(ctx context.Context, name, wantHash string) error {
	var revoked bool
	var hash string
	e := s.DB.QueryRowContext(ctx, "SELECT revoked,certificate_hash FROM identities WHERE name=?", name).Scan(&revoked, &hash)
	if e != nil || revoked {
		return Fail("NODE_UNAUTHORIZED", "Nodeは未承認または失効済みです", nil)
	}
	if wantHash != "" && hash != "" && hash != wantHash {
		return Fail("NODE_UNAUTHORIZED", "Nodeは未承認または失効済みです", nil)
	}
	return nil
}

func (s *Store) UpdateIdentityCertificate(ctx context.Context, name string, certPEM []byte) error {
	if !ValidName(name) || len(certPEM) == 0 {
		return Fail("NODE_UNAUTHORIZED", "Nodeは未承認または失効済みです", nil)
	}
	r, e := s.DB.ExecContext(ctx, "UPDATE identities SET certificate_hash=? WHERE name=? AND revoked=0", Hash(certPEM), name)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return Fail("NODE_UNAUTHORIZED", "Nodeは未承認または失効済みです", nil)
	}
	return nil
}
func (s *Store) Revoke(ctx context.Context, name string) error {
	r, e := s.DB.ExecContext(ctx, "UPDATE identities SET revoked=1 WHERE name=?", name)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return fmt.Errorf("node not found")
	}
	_, e = s.DB.ExecContext(ctx, "UPDATE nodes SET drained=1 WHERE name=?", name)
	return e
}
