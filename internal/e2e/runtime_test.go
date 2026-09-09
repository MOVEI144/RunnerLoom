//go:build linux

// Package e2e_test exercises the real controller/agent/libvirt stack. GitHub's
// external queue and the job payload are explicit fixtures, NOT live GitHub E2E.
package e2e_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/control"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	gh "github.com/MOVEI144/RunnerLoom/internal/github"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/actions/scaleset"
)

// Only the workload is replaced. The complete underlying libvirt provider,
// ownership verification, filesystem, network and deletion paths remain real.
// This adapter exists exclusively in tests, never in the Node protocol.
type diagnosticWorkload struct{ *host.Libvirt }

func (p *diagnosticWorkload) Ensure(ctx context.Context, a core.Instance, jit string) error {
	if jit != "E2E_DIAGNOSTIC_ONLY" {
		return fmt.Errorf("not a diagnostic fixture")
	}
	return p.EnsureDiagnostic(ctx, a)
}

type githubFixture struct{ next int }

func (*githubFixture) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return nil, nil
}
func (*githubFixture) RemoveRunner(context.Context, int64) error { return nil }
func (f *githubFixture) GenerateJitRunnerConfig(_ context.Context, q *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.next, Name: q.Name, RunnerScaleSetID: set}, EncodedJITConfig: "E2E_DIAGNOSTIC_ONLY"}, nil
}
func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	f, e := os.Open(path)
	require(t, e)
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	require(t, e)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
func TestControllerAgentRealVMLifecycle(t *testing.T) {
	if os.Getenv("RUNNERLOOM_REAL_RUNTIME_E2E") != "1" {
		t.Skip("explicit real-VM qualification was not requested")
	}
	if os.Geteuid() != 0 || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("this qualification modifies only disposable GitHub-owned CI hosts")
	}
	image := os.Getenv("RUNNERLOOM_E2E_IMAGE")
	if !filepath.IsAbs(image) {
		t.Fatal("absolute verified golden image path required")
	}
	evidence := os.Getenv("RUNNERLOOM_E2E_EVIDENCE")
	if !filepath.IsAbs(evidence) {
		t.Fatal("absolute evidence directory required")
	}
	require(t, os.MkdirAll(evidence, 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cluster := "e2e-" + core.ID()[:12]
	state := filepath.Join("/var/lib", cluster)
	require(t, core.PrivateDir(state))
	controllerDir := filepath.Join(state, "controller")
	agentDir := filepath.Join(state, "node")
	disks := filepath.Join("/var/lib/libvirt/images", cluster)
	digest := fileDigest(t, image)
	c := core.Example()
	c.Name = cluster
	c.Pools = c.Pools[:1]
	c.Reservations = nil
	p := &c.Pools[0]
	p.VCPU = 1
	p.MemoryMiB = 1024
	p.OverheadMiB = 512
	p.RootGiB = 20
	p.ScratchGiB = 0
	p.DiskOverheadGiB = 2
	p.MaxRunners = 1
	p.WarmIdle = 0
	p.ExecutionMinutes = 10
	p.RunnerName = cluster
	budget := core.Resources{CPU: 1, Memory: 2048, Disk: 32}
	c.Nodes = []core.Node{{Name: "node-a", Budget: budget, LocalCeiling: budget, AllowedPools: []string{p.Name}}}
	c.Images[0].Digest = digest
	s, e := core.OpenStore(controllerDir)
	require(t, e)
	var server *httptest.Server
	defer func() {
		if server != nil {
			server.Close()
		}
		s.Close()
	}()
	plan, e := s.Plan(ctx, c)
	require(t, e)
	_, e = s.Apply(ctx, plan.ID)
	require(t, e)
	ca, e := core.InitCA(controllerDir, cluster)
	require(t, e)
	src, e := os.Open(image)
	require(t, e)
	cache := &host.Images{Dir: filepath.Join(controllerDir, "images"), LimitGiB: 8, Exec: host.SystemExecutor{}}
	_, e = cache.Import(ctx, src, digest)
	src.Close()
	require(t, e)
	startServer := func() {
		handler := control.New(s, ca, cluster)
		cfg, err := handler.TLSConfig([]string{"127.0.0.1"})
		require(t, err)
		cert, err := ca.ServerCertificate([]string{"127.0.0.1"})
		require(t, err)
		cfg.Certificates = []tls.Certificate{cert}
		server = httptest.NewUnstartedServer(handler.Handler())
		server.TLS = cfg
		server.StartTLS()
	}
	startServer()
	conf := agent.Config{Node: "node-a", Cluster: cluster, Controller: server.URL, StateDir: agentDir, DiskDir: disks, NetworkCIDR: "172.30.242.0/24", Ceiling: budget, CacheGiB: 8, QEMUUser: "libvirt-qemu"}
	inv, e := s.Invite(ctx, ca, server.URL, 10*time.Minute)
	require(t, e)
	pending, e := agent.Enroll(ctx, inv, conf)
	require(t, e)
	if pending.Status != "PendingApproval" {
		t.Fatal("enrollment bypassed approval")
	}
	_, e = s.Approve(ctx, pending.ID, ca)
	require(t, e)
	approved, e := agent.Enroll(ctx, inv, conf)
	require(t, e)
	if approved.Status != "Approved" {
		t.Fatal("certificate not delivered")
	}
	a, e := agent.New(conf)
	require(t, e)
	lib, ok := a.Provider.(*host.Libvirt)
	if !ok {
		t.Fatal("not the production libvirt provider")
	}
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		lib.Emulator = "tcg"
	}
	require(t, lib.Init(ctx))
	require(t, lib.Network.Apply(ctx))
	np, e := lib.Network.Plan()
	require(t, e)
	// A known-live host endpoint gives the negative firewall probe a positive
	// control; a connection to a nonexistent host would not prove isolation.
	canary, e := net.Listen("tcp", "0.0.0.0:0")
	require(t, e)
	defer canary.Close()
	port := canary.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			cn, err := canary.Accept()
			if err != nil {
				return
			}
			cn.Close()
		}
	}()
	positive, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	require(t, e)
	positive.Close()
	lib.DiagnosticProbe = fmt.Sprintf("172.30.242.1:%d", port)
	a.Provider = &diagnosticWorkload{lib}
	a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ids := []string{}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		for _, id := range ids {
			if err := lib.Stop(cleanup, id); err != nil {
				t.Log("cleanup stop:", err)
			}
			if err := lib.Delete(cleanup, id); err != nil {
				t.Log("cleanup delete:", err)
			}
		}
		a.Client.HTTP.CloseIdleConnections()
		lib.Close()
		// Never operate on a network with changed ownership/policy.
		if lib.Network.Check(cleanup) == nil {
			for _, args := range [][]string{{"net-destroy", np.Name}, {"net-undefine", np.Name}} {
				if _, err := lib.Exec.Run(cleanup, "virsh", append([]string{"-c", "qemu:///system"}, args...), nil); err != nil {
					t.Error("network cleanup:", err)
				}
			}
			if _, err := lib.Exec.Run(cleanup, "nft", []string{"delete", "table", "inet", np.Table}, nil); err != nil {
				t.Error("firewall cleanup:", err)
			}
		}
	}()
	results := []map[string]any{}
	fixture := &githubFixture{}
	for round := 0; round < 2; round++ {
		if round == 1 {
			// Reopen persisted controller state and reconnect the real Agent using the
			// same certificate and stored sequence, not synthetic observations.
			a.Client.HTTP.CloseIdleConnections()
			server.Close()
			server = nil
			require(t, s.Close())
			s, e = core.OpenStore(controllerDir)
			require(t, e)
			startServer()
			conf.Controller = server.URL
			next, err := agent.New(conf)
			require(t, err)
			next.Provider = a.Provider
			next.Log = a.Log
			a = next
		}
		for i := 0; i < 2; i++ {
			require(t, a.Step(ctx))
		}
		rows, err := s.Explain(ctx, p.Name)
		require(t, err)
		if len(rows) != 1 || len(rows[0].Reasons) != 0 {
			t.Fatalf("node not genuinely ready: %+v", rows)
		}
		require(t, s.RefreshDemand(ctx, p.Name, 1))
		require(t, gh.Reconcile(ctx, s, *p, 1, fixture))
		runs, err := s.Instances(ctx)
		require(t, err)
		var instance core.Instance
		for _, r := range runs {
			if r.State != "Deleted" {
				instance = r
			}
		}
		if instance.ID == "" {
			t.Fatal("scheduler did not reserve a VM")
		}
		ids = append(ids, instance.ID)
		// Retrying the durable allocation request must return exactly this instance.
		again, err := s.Allocate(ctx, instance.RequestID, p.Name)
		require(t, err)
		if again.ID != instance.ID {
			t.Fatal("duplicate allocation on replay")
		}
		started := false
		completed := false
		deleted := false
		var log []byte
		for time.Now().Before(instance.Deadline) && ctx.Err() == nil {
			log, _ = os.ReadFile(filepath.Join(agentDir, "logs", instance.ID+".log"))
			// These are explicit simulated GitHub messages, not external service proof.
			event := func(end bool) {
				m := &scaleset.RunnerScaleSetMessage{MessageID: round*2 + 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
				if end {
					m.MessageID++
					m.Statistics.TotalAssignedJobs = 0
					m.JobCompletedMessages = []*scaleset.JobCompleted{{RunnerName: instance.Name(), Result: "success"}}
				} else {
					m.JobStartedMessages = []*scaleset.JobStarted{{RunnerName: instance.Name()}}
				}
				require(t, gh.PersistThenAcknowledge(ctx, s, "diagnostic-fixture", *p, m, func(context.Context, int) error { return nil }, func(context.Context, []int64) ([]int64, error) { return nil, nil }))
			}
			if strings.Contains(string(log), "RUNNERLOOM_REAL_VM_STARTED") && !started {
				event(false)
				started = true
			}
			if strings.Contains(string(log), "RUNNERLOOM_DIAGNOSTIC_EXIT=0") && !completed {
				event(true)
				completed = true
			}
			require(t, a.Step(ctx))
			runs, err = s.Instances(ctx)
			require(t, err)
			for _, r := range runs {
				if r.ID == instance.ID && r.State == "Deleted" {
					if !r.Held.Empty() {
						t.Fatal("deleted VM retained a resource commitment")
					}
					deleted = true
				}
			}
			if deleted {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Second):
			}
		}
		log, _ = os.ReadFile(filepath.Join(agentDir, "logs", instance.ID+".log"))
		require(t, os.WriteFile(filepath.Join(evidence, fmt.Sprintf("guest-%d.log", round+1)), log, 0600))
		for _, marker := range []string{"RUNNERLOOM_DIAGNOSTIC_EXIT=0", "RUNNERLOOM_CPU_AND_DISK_JOB_OK", "RUNNERLOOM_PUBLIC_HTTPS_OK", "RUNNERLOOM_HOST_PROBE_BLOCKED", "RUNNERLOOM_GITHUB_RUNNER_PRESENT"} {
			if !strings.Contains(string(log), marker) {
				t.Fatalf("guest missing %s; inspect uploaded serial log", marker)
			}
		}
		if !deleted || !started || !completed {
			t.Fatal("runtime chain did not finish", deleted, started, completed)
		}
		if _, err = os.Lstat(filepath.Join(disks, instance.ID)); !os.IsNotExist(err) {
			t.Fatal("job disk directory still present", err)
		}
		out, err := lib.Exec.Run(ctx, "virsh", []string{"-c", "qemu:///system", "list", "--all", "--name"}, nil)
		require(t, err)
		if strings.Contains(string(out), instance.Name()) {
			t.Fatal("libvirt domain remains after deletion")
		}
		results = append(results, map[string]any{"instance": instance.ID, "deleted": true, "resource_commitments_released": true, "guest_diagnostic_succeeded": true, "allocation_replay_preserved_id": true, "controller_reopened_before_round": round == 1})
	}
	if ids[0] == ids[1] {
		t.Fatal("VM identity reused across jobs")
	}
	if after := fileDigest(t, image); after != digest {
		t.Fatal("source golden image changed")
	}
	report := map[string]any{"commit": os.Getenv("GITHUB_SHA"), "scope": "real controller/agent/libvirt; GitHub queue and job are explicit fixtures", "live_github_e2e": false, "home_cluster_e2e": false, "real_virtual_machines": true, "golden_image_unchanged": true, "mutual_tls_enrollment": true, "rounds": results, "emulator": lib.Emulator}
	data, err := json.MarshalIndent(report, "", "  ")
	require(t, err)
	require(t, os.WriteFile(filepath.Join(evidence, "runtime-result.json"), data, 0600))
	t.Log(string(data))
}
