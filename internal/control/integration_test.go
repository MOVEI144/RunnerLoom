package control_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/control"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	gh "github.com/MOVEI144/RunnerLoom/internal/github"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/actions/scaleset"
)

// The hypervisor and GitHub service are explicit test doubles in this suite.
// TLS, HTTP handlers, the agent loop, image digest checks and SQLite are real.
// A separate CI job boots real libvirt VMs.
type fakeProvider struct {
	mu      sync.Mutex
	states  map[string]string
	created int
}

func (p *fakeProvider) Ready(context.Context) error { return nil }
func (p *fakeProvider) Ensure(_ context.Context, a core.Instance, jit string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if jit != "TEST_JIT_ONLY" {
		return errors.New("unexpected test JIT")
	}
	if _, ok := p.states[a.ID]; !ok {
		p.created++
		p.states[a.ID] = "Running"
	}
	return nil
}
func (p *fakeProvider) Stop(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[id] = "Stopped"
	return nil
}
func (p *fakeProvider) Delete(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states[id] != "Stopped" {
		return errors.New("test driver refused running deletion")
	}
	p.states[id] = "Deleted"
	return nil
}
func (p *fakeProvider) Inventory(context.Context) ([]core.VMReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []core.VMReport{}
	for id, state := range p.states {
		out = append(out, core.VMReport{ID: id, State: state})
	}
	return out, nil
}

type fakeImageInspector struct{}

func (fakeImageInspector) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	if name != "qemu-img" || len(args) == 0 || args[0] != "info" {
		return nil, errors.New("test image inspector cannot run host commands")
	}
	return []byte(`{"format":"qcow2","virtual-size":21474836480}`), nil
}

type fakeGitHub struct {
	next int
	mu   sync.Mutex
}

func (f *fakeGitHub) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return nil, nil
}
func (f *fakeGitHub) RemoveRunner(context.Context, int64) error { return nil }
func (f *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, q *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.next, Name: q.Name, RunnerScaleSetID: set}, EncodedJITConfig: "TEST_JIT_ONLY"}, nil
}

type lab struct {
	s     *core.Store
	c     core.Config
	ca    core.CA
	http  *httptest.Server
	dir   string
	image []byte
}

func newLab(t *testing.T) *lab {
	t.Helper()
	dir := t.TempDir()
	s, e := core.OpenStore(filepath.Join(dir, "controller"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	c := core.Example()
	image := []byte("EXPLICIT TEST IMAGE; NOT A REAL VM IMAGE")
	c.Images[0].Digest = "sha256:" + core.Hash(image)
	plan, e := s.Plan(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(context.Background(), plan.ID); e != nil {
		t.Fatal(e)
	}
	ca, e := core.InitCA(s.Dir, c.Name)
	if e != nil {
		t.Fatal(e)
	}
	server := control.New(s, ca, c.Name)
	cfg, e := server.TLSConfig([]string{"127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	cert, e := ca.ServerCertificate([]string{"127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	cfg.Certificates = []tls.Certificate{cert}
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.TLS = cfg
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	path := filepath.Join(s.Dir, "images", strings.TrimPrefix(c.Images[0].Digest, "sha256:")+".qcow2")
	if e = core.WritePrivate(path, image); e != nil {
		t.Fatal(e)
	}
	return &lab{s, c, ca, httpServer, dir, image}
}
func (l *lab) node(t *testing.T, name string, ceiling core.Resources) (*agent.Agent, *fakeProvider) {
	t.Helper()
	ctx := context.Background()
	conf := agent.Config{Node: name, Cluster: l.c.Name, Controller: l.http.URL, StateDir: filepath.Join(l.dir, name), DiskDir: filepath.Join(l.dir, name+"-disks"), NetworkCIDR: "172.30.240.0/24", Ceiling: ceiling, CacheGiB: 2, QEMUUser: "libvirt-qemu"}
	if e := os.Mkdir(conf.DiskDir, 0700); e != nil {
		t.Fatal(e)
	}
	inv, e := l.s.Invite(ctx, l.ca, l.http.URL, 10*time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	pending, e := agent.Enroll(ctx, inv, conf)
	if e != nil {
		t.Fatal(e)
	}
	if pending.Status != "PendingApproval" {
		t.Fatal("node bypassed manual approval")
	}
	if _, e = l.s.Approve(ctx, pending.ID, l.ca); e != nil {
		t.Fatal(e)
	}
	approved, e := agent.Enroll(ctx, inv, conf)
	if e != nil || approved.Status != "Approved" {
		t.Fatal("node could not collect approved certificate", e)
	}
	a, e := agent.New(conf)
	if e != nil {
		t.Fatal(e)
	}
	p := &fakeProvider{states: map[string]string{}}
	a.Provider = p
	a.Images = &host.Images{Dir: filepath.Join(conf.StateDir, "images"), LimitGiB: 2, Exec: fakeImageInspector{}}
	a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.FreeDisk = func(string) (int64, error) { return 1000, nil }
	t.Cleanup(a.Client.HTTP.CloseIdleConnections)
	return a, p
}
func step(t *testing.T, a *agent.Agent) {
	t.Helper()
	if e := a.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestEphemeralLifecycleOverMutualTLS(t *testing.T) {
	l := newLab(t)
	a, p := l.node(t, "node-a", l.c.Nodes[0].Budget)
	step(t, a)
	step(t, a)
	if e := l.s.RefreshDemand(context.Background(), "linux-lite", 1); e != nil {
		t.Fatal(e)
	}
	api := &fakeGitHub{}
	if e := gh.Reconcile(context.Background(), l.s, l.c.Pools[0], 1, api); e != nil {
		t.Fatal(e)
	}
	step(t, a)
	step(t, a)
	runs, e := l.s.Instances(context.Background())
	if e != nil || len(runs) != 1 || runs[0].State != "Idle" || p.created != 1 {
		t.Fatal("provisioning chain failed", runs, e)
	}
	instance := runs[0]
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: instance.Name()}}}
	ack := false
	e = gh.PersistThenAcknowledge(context.Background(), l.s, "test-session", l.c.Pools[0], msg, func(context.Context, int) error {
		current, _ := l.s.Instances(context.Background())
		if current[0].State != "Busy" {
			t.Fatal("ACK preceded durable job-start state")
		}
		ack = true
		return nil
	}, func(context.Context, []int64) ([]int64, error) { return nil, nil })
	if e != nil || !ack {
		t.Fatal("message processing failed", e)
	}
	msg = &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: instance.Name(), Result: "success"}}}
	if e = gh.PersistThenAcknowledge(context.Background(), l.s, "test-session", l.c.Pools[0], msg, func(context.Context, int) error { return nil }, func(context.Context, []int64) ([]int64, error) { return nil, nil }); e != nil {
		t.Fatal(e)
	}
	runs, _ = l.s.Instances(context.Background())
	if runs[0].Held.Empty() {
		t.Fatal("GitHub completion prematurely released host resources")
	}
	p.Stop(context.Background(), instance.ID)
	step(t, a)
	step(t, a)
	runs, _ = l.s.Instances(context.Background())
	if runs[0].State != "Deleted" || !runs[0].Held.Empty() || runs[0].Result != "success" {
		t.Fatal("cleanup chain did not finish", runs)
	}
	if p.created != 1 {
		t.Fatal("duplicate VM creation")
	}
}
func TestTwoNodesRespectHardReservation(t *testing.T) {
	l := newLab(t)
	a, _ := l.node(t, "node-a", l.c.Nodes[0].Budget)
	b, _ := l.node(t, "node-b", core.Resources{CPU: 8, Memory: 16384, Disk: 100})
	step(t, a)
	step(t, a)
	step(t, b)
	step(t, b)
	for i := 0; i < 3; i++ {
		if _, e := l.s.Allocate(context.Background(), core.ID(), "linux-lite"); e != nil {
			t.Fatal(e)
		}
	}
	heavy, e := l.s.Allocate(context.Background(), core.ID(), "linux-heavy")
	if e != nil {
		t.Fatal("general jobs stole dedicated capacity", e)
	}
	if heavy.Node != "node-a" || heavy.Reservation != "heavy-reserved" {
		t.Fatal("reservation placed incorrectly", heavy)
	}
}
func TestRevocationAndNodeIdentityChecks(t *testing.T) {
	l := newLab(t)
	a, _ := l.node(t, "node-a", l.c.Nodes[0].Budget)
	step(t, a)
	var out core.SyncResponse
	bad := core.Observation{Node: "someone-else", BootID: "test", Sequence: 100, Ceiling: l.c.Nodes[0].Budget}
	if e := a.Client.Post(context.Background(), "/v1/sync", bad, &out); e == nil {
		t.Fatal("node spoofed another identity")
	}
	if e := l.s.Revoke(context.Background(), "node-a"); e != nil {
		t.Fatal(e)
	}
	if e := a.Step(context.Background()); e == nil {
		t.Fatal("revocation did not affect existing TLS connection")
	}
}
func TestWrongCAIsRejected(t *testing.T) {
	l := newLab(t)
	other, e := core.InitCA(filepath.Join(l.dir, "other-ca"), "other")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = core.ClientTLS(l.ca.PEM, core.Hash(other.Certificate.Raw), "127.0.0.1", nil); e == nil {
		t.Fatal("wrong fingerprint accepted")
	}
}
func TestUnauthenticatedClientCannotSync(t *testing.T) {
	l := newLab(t)
	client := l.http.Client()
	body := strings.NewReader(`{"node":"node-a"}`)
	resp, e := client.Post(l.http.URL+"/v1/sync", "application/json", body)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unauthenticated sync returned %d", resp.StatusCode)
	}
}
func TestPublicAPIHasNoAdministrationRoute(t *testing.T) {
	l := newLab(t)
	resp, e := l.http.Client().Post(l.http.URL+"/v1/config/apply", "application/json", strings.NewReader(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("unexpected administrative API exposure")
	}
}
func TestConfigAndPrivateKeyNeverAppearInStatus(t *testing.T) {
	l := newLab(t)
	b, e := json.Marshal(l.c)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatal("secret key entered configuration")
	}
}
