package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Store struct {
	DB  *sql.DB
	Dir string
	Now func() time.Time
	box cipher.AEAD
}

func PrivateDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("private directory must be a normalized absolute path")
	}
	for p := path; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in private path")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if e := os.MkdirAll(path, 0700); e != nil {
		return e
	}
	st, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private directory must have mode 0700")
	}
	return nil
}
func ReadSecret(path string) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > MaxJSON {
		return nil, errors.New("secret must be a regular owner-only file smaller than 1MiB")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var b strings.Builder
	_, e = ioCopyBounded(&b, f, MaxJSON)
	return []byte(b.String()), e
}
func WritePrivate(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := PrivateDir(dir); e != nil {
		return e
	}
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return errors.New("refusing non-regular destination")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	f, e := os.CreateTemp(dir, ".new-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if os.Geteuid() == 0 {
		if parent, e := os.Stat(dir); e == nil {
			if stat, ok := parent.Sys().(*syscall.Stat_t); ok {
				if e = f.Chown(int(stat.Uid), int(stat.Gid)); e != nil {
					f.Close()
					return e
				}
			}
		}
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func OpenStore(dir string) (*Store, error) {
	if e := PrivateDir(dir); e != nil {
		return nil, e
	}
	keyPath := filepath.Join(dir, "master.key")
	key, e := ReadSecret(keyPath)
	if os.IsNotExist(e) {
		if st, dbErr := os.Stat(filepath.Join(dir, "controller.db")); dbErr == nil && st.Size() > 0 {
			return nil, Fail("MASTER_KEY_MISSING", "既存DBの復号鍵がありません。バックアップから復旧してください", nil)
		} else if dbErr != nil && !os.IsNotExist(dbErr) {
			return nil, dbErr
		}
		key = make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			return nil, e
		}
		f, ce := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if ce != nil {
			return nil, ce
		}
		_, ce = f.Write(key)
		if ce == nil {
			ce = f.Sync()
		}
		f.Close()
		if ce != nil {
			return nil, ce
		}
	} else if e != nil {
		return nil, e
	}
	if len(key) != 32 {
		return nil, errors.New("master key must be exactly 32 bytes")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	box, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	dbPath := filepath.Join(dir, "controller.db")
	f, e := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e == nil {
		f.Close()
	} else if !os.IsExist(e) {
		return nil, e
	}
	st, e := os.Lstat(dbPath)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe database path or permissions")
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	for _, p := range []string{"busy_timeout(10000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Dir: dir, Now: time.Now, box: box}
	if e = s.init(); e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) init() error {
	var version int
	if e := s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return e
	}
	if version > 1 {
		return errors.New("database was created by a newer RunnerLoom")
	}
	_, e := s.DB.Exec(`CREATE TABLE IF NOT EXISTS settings(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL,payload BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS plans(id TEXT PRIMARY KEY,base INTEGER NOT NULL,expires INTEGER NOT NULL,payload BLOB NOT NULL,applied INTEGER);
 CREATE TABLE IF NOT EXISTS instances(id TEXT PRIMARY KEY,request TEXT NOT NULL UNIQUE,payload BLOB NOT NULL,jit BLOB);
 CREATE TABLE IF NOT EXISTS nodes(name TEXT PRIMARY KEY,payload BLOB NOT NULL,drained INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS demand(pool TEXT PRIMARY KEY,payload BLOB NOT NULL,barrier INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS inbox(session TEXT NOT NULL,id INTEGER NOT NULL,hash TEXT NOT NULL,created INTEGER NOT NULL,PRIMARY KEY(session,id));
 CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY AUTOINCREMENT,at INTEGER NOT NULL,event TEXT NOT NULL,target TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS invites(id TEXT PRIMARY KEY,hash TEXT NOT NULL,expires INTEGER NOT NULL,used INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS enrollments(id TEXT PRIMARY KEY,invite TEXT NOT NULL UNIQUE,name TEXT NOT NULL,csr BLOB NOT NULL,ceiling BLOB NOT NULL,status TEXT NOT NULL,certificate BLOB);
 CREATE TABLE IF NOT EXISTS identities(name TEXT PRIMARY KEY,certificate_hash TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
 PRAGMA user_version=1;`)
	return e
}
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Obtain the SQLite writer lock BEFORE reading capacity, including between processes.
	if _, e = tx.ExecContext(ctx, "UPDATE settings SET revision=revision WHERE id=1"); e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readConfig(ctx context.Context, q rowReader) (Config, int64, error) {
	var c Config
	var r int64
	var b []byte
	e := q.QueryRowContext(ctx, "SELECT revision,payload FROM settings WHERE id=1").Scan(&r, &b)
	if errors.Is(e, sql.ErrNoRows) {
		return c, 0, nil
	}
	if e != nil {
		return c, 0, e
	}
	e = json.Unmarshal(b, &c)
	return c, r, e
}
func (s *Store) Config(ctx context.Context) (Config, int64, error) { return readConfig(ctx, s.DB) }

type Plan struct {
	ID          string    `json:"id"`
	Base        int64     `json:"baseRevision"`
	Expires     time.Time `json:"expires"`
	Config      Config    `json:"config"`
	HostChanges bool      `json:"hostChanges"`
}

func mergeConfig(old, next Config) Config {
	for _, v := range old.Nodes {
		if _, ok := next.Node(v.Name); !ok {
			next.Nodes = append(next.Nodes, v)
		}
	}
	for _, v := range old.Pools {
		if _, ok := next.Pool(v.Name); !ok {
			next.Pools = append(next.Pools, v)
		}
	}
	for _, v := range old.Images {
		if _, ok := next.Image(v.Name); !ok {
			next.Images = append(next.Images, v)
		}
	}
	for _, v := range old.Reservations {
		found := false
		for _, n := range next.Reservations {
			if n.Name == v.Name {
				found = true
			}
		}
		if !found {
			next.Reservations = append(next.Reservations, v)
		}
	}
	return next
}
func (s *Store) Plan(ctx context.Context, c Config) (p Plan, err error) {
	if err = c.Validate(); err != nil {
		return p, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		old, r, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		if r > 0 && old.Name != c.Name {
			return Fail("CLUSTER_MISMATCH", "別のClusterに上書きできません", nil)
		}
		c = mergeConfig(old, c)
		if e = c.Validate(); e != nil {
			return e
		}
		p = Plan{ID: ID(), Base: r, Expires: s.Now().UTC().Add(10 * time.Minute), Config: c}
		b, _ := json.Marshal(c)
		_, e = tx.ExecContext(ctx, "INSERT INTO plans(id,base,expires,payload) VALUES(?,?,?,?)", p.ID, r, p.Expires.Unix(), b)
		return e
	})
	return
}
func (s *Store) Apply(ctx context.Context, id string) (revision int64, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var base, expires int64
		var applied sql.NullInt64
		var b []byte
		if e := tx.QueryRowContext(ctx, "SELECT base,expires,payload,applied FROM plans WHERE id=?", id).Scan(&base, &expires, &b, &applied); e != nil {
			return Fail("PLAN_NOT_FOUND", "変更計画がありません", nil)
		}
		if applied.Valid {
			revision = applied.Int64
			return nil
		}
		if s.Now().Unix() >= expires {
			return Fail("PLAN_EXPIRED", "変更計画を作り直してください", nil)
		}
		_, r, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		if r != base {
			return Fail("REVISION_CONFLICT", "計画後に設定が変更されています", nil)
		}
		var c Config
		if e = Decode(strings.NewReader(string(b)), &c); e != nil {
			return e
		}
		if e = c.Validate(); e != nil {
			return e
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		if e = checkCapacityUpdate(c, runs); e != nil {
			return e
		}
		revision = r + 1
		_, e = tx.ExecContext(ctx, "INSERT INTO settings(id,revision,payload) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,payload=excluded.payload", revision, b)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE plans SET applied=? WHERE id=?", revision, id)
		return e
	})
	return
}
func checkCapacityUpdate(c Config, runs []Instance) error {
	if e := validateReservationOwners(c, runs); e != nil {
		return e
	}
	for _, p := range c.Pools {
		count := int64(0)
		for _, a := range runs {
			if a.Pool.Name == p.Name && !a.Held.Empty() {
				count++
			}
		}
		if count > p.MaxRunners {
			return Fail("POOL_LIMIT_BUSY", "使用中の台数よりPool上限を小さくできません", p.Name)
		}
	}
	for _, n := range c.Nodes {
		held := Resources{}
		for _, a := range runs {
			if a.Node == n.Name {
				held = held.Add(a.Held)
				if !a.Held.Empty() {
					p, ok := c.Pool(a.Pool.Name)
					im, _ := c.Image(p.Image)
					if !ok || p.Charge() != a.Pool.Charge() || im.Digest != a.Image.Digest {
						return Fail("POOL_IN_USE", "使用中Poolのサイズ・イメージを変更できません", a.Pool.Name)
					}
				}
			}
		}
		for _, r := range c.Reservations {
			if r.Node != n.Name {
				continue
			}
			protected, _, e := reservationProtection(c, r, runs)
			if e != nil {
				return e
			}
			held = held.Add(protected)
		}
		if !held.Fits(n.Budget) {
			return Fail("BUDGET_BUSY", "割り当て済み・予約済みの資源を下回っています", n.Name)
		}
	}
	return nil
}

type queryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readInstances(ctx context.Context, q queryReader) ([]Instance, error) {
	rows, e := q.QueryContext(ctx, "SELECT payload FROM instances ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	a := []Instance{}
	for rows.Next() {
		var b []byte
		var v Instance
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		a = append(a, v)
	}
	return a, rows.Err()
}
func (s *Store) Instances(ctx context.Context) ([]Instance, error) { return readInstances(ctx, s.DB) }
func saveInstance(ctx context.Context, tx *sql.Tx, a Instance) error {
	b, _ := json.Marshal(a)
	_, e := tx.ExecContext(ctx, "UPDATE instances SET payload=? WHERE id=?", b, a.ID)
	return e
}
func (s *Store) SetJIT(ctx context.Context, id string, runnerID int64, jit string) error {
	if len(jit) == 0 || len(jit) > 131072 || runnerID <= 0 {
		return errors.New("invalid JIT response")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var b []byte
		if e := tx.QueryRowContext(ctx, "SELECT payload FROM instances WHERE id=?", id).Scan(&b); e != nil {
			return e
		}
		var a Instance
		if e := json.Unmarshal(b, &a); e != nil {
			return e
		}
		if a.JITReady {
			return nil
		}
		if a.State != "Reserved" {
			return errors.New("instance no longer awaiting registration")
		}
		nonce := make([]byte, s.box.NonceSize())
		if _, e := rand.Read(nonce); e != nil {
			return e
		}
		encrypted := s.box.Seal(nonce, nonce, []byte(jit), []byte(id))
		a.JITReady = true
		a.RunnerID = runnerID
		a.State = "Provisioning"
		a.Updated = s.Now().UTC()
		if e := saveInstance(ctx, tx, a); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE instances SET jit=? WHERE id=?", encrypted, id)
		return e
	})
}
func (s *Store) decrypt(id string, b []byte) (string, error) {
	n := s.box.NonceSize()
	if len(b) < n {
		return "", errors.New("missing encrypted JIT")
	}
	p, e := s.box.Open(nil, b[:n], b[n:], []byte(id))
	return string(p), e
}

type Candidate struct {
	Node        string    `json:"node"`
	Available   Resources `json:"available"`
	Reservation string    `json:"reservation,omitempty"`
	Reasons     []string  `json:"reasons"`
}

func candidates(c Config, p Pool, runs []Instance, obs map[string]Observation, now time.Time) []Candidate {
	reservationErr := validateReservationOwners(c, runs)
	rows := []Candidate{}
	count := int64(0)
	for _, a := range runs {
		if a.Pool.Name == p.Name && !a.Held.Empty() {
			count++
		}
	}
	im, _ := c.Image(p.Image)
	for _, n := range c.Nodes {
		v := Candidate{Node: n.Name, Available: n.Budget, Reasons: []string{}}
		if reservationErr != nil {
			v.Reasons = append(v.Reasons, "RESERVATION_INCONSISTENT")
		}
		held := Resources{}
		unused := Resources{}
		if !p.Enabled {
			v.Reasons = append(v.Reasons, "POOL_DISABLED")
		}
		if !c.Eligible(p, n) {
			v.Reasons = append(v.Reasons, "NODE_NOT_ALLOWED")
		}
		if count >= p.MaxRunners {
			v.Reasons = append(v.Reasons, "POOL_LIMIT")
		}
		o, ok := obs[n.Name]
		if !ok || !o.Ready || o.Drained || now.Sub(o.Seen) > 45*time.Second || now.Before(o.Seen) {
			v.Reasons = append(v.Reasons, "NODE_NOT_READY")
		}
		if !o.Isolation {
			v.Reasons = append(v.Reasons, "NETWORK_NOT_VERIFIED")
		}
		if !n.Budget.Fits(o.Ceiling) {
			v.Reasons = append(v.Reasons, "LOCAL_CEILING")
		}
		if !Contains(o.Digests, im.Digest) {
			v.Reasons = append(v.Reasons, "IMAGE_MISSING")
		}
		for _, a := range runs {
			if a.Node == n.Name {
				held = held.Add(a.Held)
			}
		}
		for _, r := range c.Reservations {
			if r.Node != n.Name {
				continue
			}
			protected, slots, e := reservationProtection(c, r, runs)
			if e != nil {
				v.Reasons = append(v.Reasons, "RESERVATION_INCONSISTENT")
				continue
			}
			unused = unused.Add(protected)
			if slots > 0 && r.Pool == p.Name && v.Reservation == "" {
				v.Reservation = r.Name
			}
		}
		v.Available = v.Available.Sub(held).Sub(unused)
		physical := o.FreeDiskGiB - 2 - held.Disk - unused.Disk
		if v.Reservation != "" {
			v.Available = v.Available.Add(p.Charge())
			physical += p.Charge().Disk
		}
		if !p.Charge().Fits(v.Available) {
			v.Reasons = append(v.Reasons, "INSUFFICIENT_BUDGET")
		}
		if physical < p.Charge().Disk {
			v.Reasons = append(v.Reasons, "INSUFFICIENT_PHYSICAL_DISK")
		}
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (len(a.Reasons) == 0) != (len(b.Reasons) == 0) {
			return len(a.Reasons) == 0
		}
		if (a.Reservation != "") != (b.Reservation != "") {
			return a.Reservation != ""
		}
		if a.Available.Memory != b.Available.Memory {
			return a.Available.Memory < b.Available.Memory
		}
		return a.Node < b.Node
	})
	return rows
}
func readNodes(ctx context.Context, tx *sql.Tx) (map[string]Observation, error) {
	rows, e := tx.QueryContext(ctx, "SELECT payload,drained FROM nodes")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]Observation{}
	for rows.Next() {
		var b []byte
		var d bool
		var o Observation
		if e = rows.Scan(&b, &d); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &o); e != nil {
			return nil, e
		}
		o.Drained = d
		out[o.Node] = o
	}
	return out, rows.Err()
}
func (s *Store) Explain(ctx context.Context, pool string) (out []Candidate, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		c, _, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		p, ok := c.Pool(pool)
		if !ok {
			return Fail("POOL_NOT_FOUND", "Poolがありません", nil)
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		obs, e := readNodes(ctx, tx)
		if e != nil {
			return e
		}
		out = candidates(c, p, runs, obs, s.Now())
		return nil
	})
	return
}
func (s *Store) Allocate(ctx context.Context, request, pool string) (out Instance, err error) {
	if request == "" || len(request) > 128 {
		return out, errors.New("invalid request ID")
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		for _, a := range runs {
			if a.RequestID == request {
				if a.Pool.Name != pool {
					return Fail("IDEMPOTENCY_CONFLICT", "同じ要求IDに異なるPoolを指定できません", nil)
				}
				out = a
				return nil
			}
		}
		c, _, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		p, ok := c.Pool(pool)
		if !ok {
			return Fail("POOL_NOT_FOUND", "Poolがありません", nil)
		}
		obs, e := readNodes(ctx, tx)
		if e != nil {
			return e
		}
		rows := candidates(c, p, runs, obs, s.Now())
		if len(rows) == 0 || len(rows[0].Reasons) > 0 {
			return Fail("NO_CAPACITY", "現在配置できるNodeがありません", rows)
		}
		im, _ := c.Image(p.Image)
		now := s.Now().UTC()
		out = Instance{ID: ID(), RequestID: request, Node: rows[0].Node, Pool: p, Image: im, State: "Reserved", Reservation: rows[0].Reservation, Held: p.Charge(), Created: now, Updated: now, Deadline: now.Add(time.Duration(p.ExecutionMinutes+10) * time.Minute)}
		b, _ := json.Marshal(out)
		_, e = tx.ExecContext(ctx, "INSERT INTO instances(id,request,payload) VALUES(?,?,?)", out.ID, request, b)
		return e
	})
	return
}

// Sync is called only after the HTTP boundary verifies the node certificate.
// Guest output never serves as deletion proof: the trusted host provider reports it.
func (s *Store) Sync(ctx context.Context, name string, o Observation) (response SyncResponse, err error) {
	response = SyncResponse{Commands: []Command{}, IntervalSeconds: 5}
	if o.Node != name || !ValidName(name) || o.Sequence < 1 || o.BootID == "" || len(o.BootID) > 128 || !o.Ceiling.Valid() || o.FreeDiskGiB < 0 || o.FreeDiskGiB > 1073741824 || len(o.Instances) > 10000 || len(o.Digests) > 4096 {
		return response, Fail("NODE_REPORT", "Node報告が不正です", nil)
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		c, _, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		n, ok := c.Node(name)
		if !ok {
			return Fail("NODE_NOT_CONFIGURED", "ControllerにNodeがありません", nil)
		}
		if !n.Budget.Fits(o.Ceiling) {
			return Fail("LOCAL_CEILING", "Nodeの承認上限を超えています", nil)
		}
		observations, e := readNodes(ctx, tx)
		if e != nil {
			return e
		}
		if old, ok := observations[name]; ok && old.Sequence >= o.Sequence {
			return Fail("STALE_REPORT", "古い報告を拒否しました", nil)
		}
		response.Images = []Image{}
		imageSeen := map[string]bool{}
		for _, p := range c.Pools {
			if c.Eligible(p, n) {
				im, _ := c.Image(p.Image)
				if !imageSeen[im.Digest] {
					response.Images = append(response.Images, im)
					imageSeen[im.Digest] = true
				}
			}
		}
		o.Seen = s.Now().UTC()
		o.Error = ""
		b, _ := json.Marshal(o)
		_, e = tx.ExecContext(ctx, "INSERT INTO nodes(name,payload) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload", name, b)
		if e != nil {
			return e
		}
		reports := map[string]VMReport{}
		for _, r := range o.Instances {
			if _, ok := reports[r.ID]; ok {
				return Fail("DUPLICATE_REPORT", "VM報告が重複しています", nil)
			}
			reports[r.ID] = r
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		for _, a := range runs {
			if a.Node != name || a.State == "Deleted" {
				continue
			}
			if r, ok := reports[a.ID]; ok {
				switch r.State {
				case "Running":
					if a.State == "Provisioning" {
						a.State = "Idle"
					}
				case "Stopped":
					if a.Result == "" && a.Held.CPU > 0 {
						if _, e = tx.ExecContext(ctx, "UPDATE demand SET barrier=1 WHERE pool=?", a.Pool.Name); e != nil {
							return e
						}
					}
					a.State = "Deleting"
					a.Held.CPU = 0
					a.Held.Memory = 0
				case "Deleted":
					if a.State != "Deleting" {
						return Fail("UNEXPECTED_DELETION", "削除指示前の削除報告です", a.ID)
					}
					a.State = "Deleted"
					a.Held = Resources{}
					if _, e = tx.ExecContext(ctx, "UPDATE instances SET jit=NULL WHERE id=?", a.ID); e != nil {
						return e
					}
				case "Failed":
					a.State = "Stopping"
				case "Unknown": // retain every commitment and phase
				default:
					return Fail("REPORT_STATE", "VM報告の状態が不正です", r.State)
				}
			}
			a.Updated = s.Now().UTC()
			if e = saveInstance(ctx, tx, a); e != nil {
				return e
			}
			action := ""
			jit := ""
			switch {
			case a.State == "Deleted":
				continue
			case a.State == "Deleting":
				action = "delete"
			case a.State == "Stopping" || !s.Now().Before(a.Deadline):
				action = "stop"
			case a.JITReady:
				if r, ok := reports[a.ID]; ok && r.State == "Running" {
					continue
				}
				action = "ensure"
				var encrypted []byte
				if e = tx.QueryRowContext(ctx, "SELECT jit FROM instances WHERE id=?", a.ID).Scan(&encrypted); e != nil {
					return e
				}
				jit, e = s.decrypt(a.ID, encrypted)
				if e != nil {
					return e
				}
			}
			if action != "" && len(response.Commands) < 4 {
				response.Commands = append(response.Commands, Command{Instance: a, Action: action, JIT: jit})
			}
		}
		return nil
	})
	return
}
func (s *Store) Drain(ctx context.Context, name string, drained bool) error {
	r, e := s.DB.ExecContext(ctx, "UPDATE nodes SET drained=? WHERE name=?", drained, name)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return Fail("NODE_NOT_OBSERVED", "Nodeはまだ接続されていません", nil)
	}
	return nil
}
func (s *Store) Nodes(ctx context.Context) (out map[string]Observation, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error { var e error; out, e = readNodes(ctx, tx); return e })
	return
}
func (s *Store) Demands(ctx context.Context) ([]Demand, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT payload,barrier FROM demand ORDER BY pool")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Demand{}
	for rows.Next() {
		var d Demand
		var b []byte
		var barrier bool
		if e = rows.Scan(&b, &barrier); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &d); e != nil {
			return nil, e
		}
		d.Blocked = barrier
		out = append(out, d)
	}
	return out, rows.Err()
}

type RunnerEvent struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Result string `json:"result,omitempty"`
}

// PersistMessage commits the statistics and events before the SDK message ACK.
func (s *Store) PersistMessage(ctx context.Context, session string, id int, pool string, desired int64, events []RunnerEvent) error {
	if desired < 0 || desired > 1000000 || id < 0 {
		return errors.New("invalid demand")
	}
	raw, _ := json.Marshal(struct {
		Pool    string
		Desired int64
		Events  []RunnerEvent
	}{pool, desired, events})
	hash := Hash(raw)
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var prev string
		e := tx.QueryRowContext(ctx, "SELECT hash FROM inbox WHERE session=? AND id=?", session, id).Scan(&prev)
		if e == nil {
			if prev != hash {
				return Fail("MESSAGE_CONFLICT", "同じメッセージIDの内容が異なります", nil)
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		for _, ev := range events {
			for _, a := range runs {
				if a.Name() != ev.Name || a.Pool.Name != pool {
					continue
				}
				if ev.State == "Busy" && a.Active() {
					a.State = "Busy"
				}
				if ev.State == "Complete" {
					a.Result = ev.Result
				}
				a.Updated = s.Now().UTC()
				if e = saveInstance(ctx, tx, a); e != nil {
					return e
				}
			}
		}
		d := Demand{Pool: pool, Desired: desired, Seen: s.Now().UTC()}
		b, _ := json.Marshal(d)
		_, e = tx.ExecContext(ctx, "INSERT INTO demand(pool,payload,barrier) VALUES(?,?,0) ON CONFLICT(pool) DO UPDATE SET payload=excluded.payload,barrier=0", pool, b)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO inbox(session,id,hash,created) VALUES(?,?,?,?)", session, id, hash, s.Now().Unix())
		return e
	})
}
func (s *Store) StopInstance(ctx context.Context, id string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		for _, a := range runs {
			if a.ID == id {
				if a.State == "Deleted" || a.State == "Deleting" {
					return nil
				}
				a.State = "Stopping"
				return saveInstance(ctx, tx, a)
			}
		}
		return Fail("INSTANCE_NOT_FOUND", "VM記録がありません", nil)
	})
}
func (s *Store) Backup(ctx context.Context, destination string) error {
	if !filepath.IsAbs(destination) {
		return errors.New("backup requires an absolute new path")
	}
	if _, e := os.Lstat(destination); !os.IsNotExist(e) {
		return errors.New("backup destination must not exist")
	}
	f, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "VACUUM INTO ?", destination)
	if e != nil {
		return e
	}
	return os.Chmod(destination, 0600)
}
func (s *Store) Audit(ctx context.Context, event, target string) error {
	if len(event) > 128 || len(target) > 256 {
		return fmt.Errorf("audit value too long")
	}
	_, e := s.DB.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), event, target)
	return e
}
