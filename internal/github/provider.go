// Package github adapts the official GitHub scale-set client. No undocumented
// protocol copies or outbound webhooks are used. Tokens never enter VM specs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

type Credentials struct {
	TokenFile      string `json:"tokenFile,omitempty"`
	ClientID       string `json:"clientID,omitempty"`
	InstallationID int64  `json:"installationID,omitempty"`
	PrivateKeyFile string `json:"privateKeyFile,omitempty"`
}

func LoadCredentials(path string) (Credentials, error) {
	var c Credentials
	b, e := core.ReadSecret(path)
	if e != nil {
		return c, e
	}
	if e = core.Decode(bytes.NewReader(b), &c); e != nil {
		return c, e
	}
	pat := c.TokenFile != ""
	app := c.ClientID != "" || c.InstallationID != 0 || c.PrivateKeyFile != ""
	if pat == app {
		return c, errors.New("choose exactly one GitHub App or PAT credential")
	}
	if pat && !filepath.IsAbs(c.TokenFile) || app && (!filepath.IsAbs(c.PrivateKeyFile) || c.ClientID == "" || c.InstallationID <= 0) {
		return c, errors.New("credential references are incomplete")
	}
	return c, nil
}

type auth struct {
	Credentials Credentials
	HTTP        *http.Client
	mu          sync.Mutex
	token       string
	expires     time.Time
}

func newAuth(c Credentials) *auth {
	return &auth{Credentials: c, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("GitHub API redirects are forbidden") }}}
}
func (a *auth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.Credentials
	if c.TokenFile != "" {
		b, e := core.ReadSecret(c.TokenFile)
		if e != nil {
			return "", e
		}
		token := strings.TrimSpace(string(b))
		if token == "" || strings.ContainsAny(token, "\r\n ") {
			return "", errors.New("invalid token file")
		}
		return token, nil
	}
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	b, e := core.ReadSecret(c.PrivateKeyFile)
	if e != nil {
		return "", e
	}
	key, e := jwt.ParseRSAPrivateKeyFromPEM(b)
	if e != nil {
		return "", errors.New("invalid GitHub App key")
	}
	now := time.Now()
	signed, e := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: c.ClientID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}).SignedString(key)
	if e != nil {
		return "", e
	}
	r, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", c.InstallationID), strings.NewReader("{}"))
	if e != nil {
		return "", e
	}
	r.Header.Set("Authorization", "Bearer "+signed)
	r.Header.Set("Accept", "application/vnd.github+json")
	resp, e := a.HTTP.Do(r)
	if e != nil {
		return "", errors.New("GitHub App token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("GitHub App token request returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, core.MaxJSON)).Decode(&out); e != nil {
		return "", errors.New("invalid GitHub token response")
	}
	if out.Token == "" || out.Expires.Before(now) {
		return "", errors.New("invalid GitHub token expiry")
	}
	a.token = out.Token
	a.expires = out.Expires
	return out.Token, nil
}
func (a *auth) get(ctx context.Context, path string, v any) error {
	token, e := a.Token(ctx)
	if e != nil {
		return e
	}
	r, e := http.NewRequestWithContext(ctx, "GET", "https://api.github.com"+path, nil)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, e := a.HTTP.Do(r)
	if e != nil {
		return errors.New("GitHub policy request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub policy request returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}
func (a *auth) CheckAccess(ctx context.Context, c core.Config) error {
	u, _ := url.Parse(c.GitHub.URL)
	scope := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, repo := range c.GitHub.AllowedRepositories {
		var r struct {
			Private  bool   `json:"private"`
			FullName string `json:"full_name"`
		}
		if e := a.get(ctx, "/repos/"+repo, &r); e != nil {
			return e
		}
		if !r.Private || !strings.EqualFold(r.FullName, repo) {
			return core.Fail("PUBLIC_REPOSITORY_DISABLED", "初期版は明示した非公開Repositoryのみ利用できます", repo)
		}
	}
	if len(scope) == 2 {
		return nil
	}
	var group struct {
		Visibility   string `json:"visibility"`
		AllowsPublic bool   `json:"allows_public_repositories"`
	}
	path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", url.PathEscape(scope[0]), c.GitHub.RunnerGroupID)
	if e := a.get(ctx, path, &group); e != nil {
		return e
	}
	if group.Visibility != "selected" || group.AllowsPublic {
		return core.Fail("RUNNER_GROUP_ACCESS", "Runner GroupをSelected repositories・非公開のみで設定してください", nil)
	}
	actual := []string{}
	for page := 1; page <= 100; page++ {
		var r struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if e := a.get(ctx, path+fmt.Sprintf("/repositories?per_page=100&page=%d", page), &r); e != nil {
			return e
		}
		for _, repo := range r.Repositories {
			actual = append(actual, strings.ToLower(repo.FullName))
		}
		if len(r.Repositories) < 100 {
			break
		}
		if page == 100 {
			return errors.New("runner-group repository pagination limit")
		}
	}
	expected := []string{}
	for _, r := range c.GitHub.AllowedRepositories {
		expected = append(expected, strings.ToLower(r))
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		return core.Fail("RUNNER_GROUP_MISMATCH", "GitHub側と設定ファイルの許可Repositoryが一致しません", nil)
	}
	return nil
}

type Binding struct {
	Pool       string `json:"pool"`
	URL        string `json:"url"`
	GroupID    int    `json:"groupID"`
	ScaleSetID int    `json:"scaleSetID"`
	RunnerName string `json:"runnerName"`
}
type JITAPI interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}
type Manager struct {
	Store    *core.Store
	Config   core.Config
	Client   *scaleset.Client
	Log      *slog.Logger
	Auth     *auth
	Bindings map[string]Binding
}

func New(s *core.Store, c core.Config) (*Manager, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	credentials, e := LoadCredentials(c.GitHub.CredentialFile)
	if e != nil {
		return nil, e
	}
	info := scaleset.SystemInfo{System: "runnerloom", Version: "0.1.0-dev", Subsystem: "controller"}
	var client *scaleset.Client
	if credentials.TokenFile != "" {
		b, e := core.ReadSecret(credentials.TokenFile)
		if e != nil {
			return nil, e
		}
		client, e = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: c.GitHub.URL, PersonalAccessToken: strings.TrimSpace(string(b)), SystemInfo: info})
		if e != nil {
			return nil, errors.New("cannot configure GitHub scale-set client")
		}
	} else {
		key, e := core.ReadSecret(credentials.PrivateKeyFile)
		if e != nil {
			return nil, e
		}
		client, e = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{GitHubConfigURL: c.GitHub.URL, GitHubAppAuth: scaleset.GitHubAppAuth{ClientID: credentials.ClientID, InstallationID: credentials.InstallationID, PrivateKey: string(key)}, SystemInfo: info})
		if e != nil {
			return nil, errors.New("cannot configure GitHub App scale-set client")
		}
	}
	return &Manager{Store: s, Config: c, Client: client, Auth: newAuth(credentials), Log: slog.Default(), Bindings: map[string]Binding{}}, nil
}
func (m *Manager) Check(ctx context.Context) error { return m.Auth.CheckAccess(ctx, m.Config) }
func (m *Manager) bind(ctx context.Context, p core.Pool) (Binding, error) {
	path := filepath.Join(m.Store.Dir, "bindings", p.Name+".json")
	b, e := core.ReadSecret(path)
	var binding Binding
	if e == nil {
		if e = core.Decode(bytes.NewReader(b), &binding); e != nil {
			return binding, e
		}
		if binding.URL != m.Config.GitHub.URL || binding.GroupID != m.Config.GitHub.RunnerGroupID || binding.RunnerName != p.RunnerName {
			return binding, errors.New("existing scale-set binding differs; create another pool")
		}
		set, e := m.Client.GetRunnerScaleSetByID(ctx, binding.ScaleSetID)
		if e != nil || set == nil {
			return binding, errors.New("cannot verify existing scale set")
		}
		if set.Name != binding.RunnerName || set.RunnerGroupID != binding.GroupID {
			return binding, errors.New("scale-set ownership mismatch")
		}
		return binding, nil
	}
	if !os.IsNotExist(e) {
		return binding, e
	}
	existing, e := m.Client.GetRunnerScaleSet(ctx, m.Config.GitHub.RunnerGroupID, p.RunnerName)
	if e != nil {
		return binding, errors.New("cannot query GitHub scale sets")
	}
	if existing != nil {
		return binding, core.Fail("SCALE_SET_EXISTS", "同名のScale Setがあります。明示的なadoptが必要です", p.RunnerName)
	}
	set, e := m.Client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: p.RunnerName, RunnerGroupID: m.Config.GitHub.RunnerGroupID, Labels: []scaleset.Label{{Name: p.RunnerName, Type: "System"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: false}})
	if e != nil || set == nil || set.ID <= 0 {
		return binding, errors.New("GitHub scale-set creation failed; inspect before retrying an ambiguous response")
	}
	binding = Binding{p.Name, m.Config.GitHub.URL, m.Config.GitHub.RunnerGroupID, set.ID, p.RunnerName}
	b, _ = json.Marshal(binding)
	if e = core.WritePrivate(path, b); e != nil {
		return binding, e
	}
	return binding, nil
}
func (m *Manager) Adopt(ctx context.Context, pool string, id int) error {
	p, ok := m.Config.Pool(pool)
	if !ok {
		return errors.New("unknown pool")
	}
	if e := m.Check(ctx); e != nil {
		return e
	}
	set, e := m.Client.GetRunnerScaleSetByID(ctx, id)
	if e != nil || set == nil || set.Name != p.RunnerName || set.RunnerGroupID != m.Config.GitHub.RunnerGroupID {
		return errors.New("scale-set identity does not match this pool")
	}
	path := filepath.Join(m.Store.Dir, "bindings", pool+".json")
	if _, e = os.Lstat(path); !os.IsNotExist(e) {
		return errors.New("pool already has a binding")
	}
	binding := Binding{pool, m.Config.GitHub.URL, m.Config.GitHub.RunnerGroupID, id, p.RunnerName}
	b, _ := json.Marshal(binding)
	return core.WritePrivate(path, b)
}

// PersistThenAcknowledge is shared by the live adapter and integration tests.
// No ACK is sent until durable demand/events have been committed.
func PersistThenAcknowledge(ctx context.Context, s *core.Store, session string, p core.Pool, msg *scaleset.RunnerScaleSetMessage, ack func(context.Context, int) error, acquire func(context.Context, []int64) ([]int64, error)) error {
	if msg == nil || msg.Statistics == nil {
		return errors.New("scale-set message has no statistics")
	}
	events := []core.RunnerEvent{}
	for _, v := range msg.JobStartedMessages {
		events = append(events, core.RunnerEvent{Name: v.RunnerName, State: "Busy"})
	}
	for _, v := range msg.JobCompletedMessages {
		events = append(events, core.RunnerEvent{Name: v.RunnerName, State: "Complete", Result: v.Result})
	}
	if e := s.PersistMessage(ctx, session, msg.MessageID, p.Name, int64(msg.Statistics.TotalAssignedJobs), events); e != nil {
		return e
	}
	// Acquisition is idempotent on GitHub's runner-request IDs. Persisting first
	// permits replay after a crash before ACK without losing the demand snapshot.
	ids := []int64{}
	for _, v := range msg.JobAvailableMessages {
		ids = append(ids, v.RunnerRequestID)
	}
	if len(ids) > 0 {
		if _, e := acquire(ctx, ids); e != nil {
			return e
		}
	}
	return ack(ctx, msg.MessageID)
}
func (m *Manager) listen(ctx context.Context, p core.Pool, b Binding) error {
	session, e := m.Client.MessageSessionClient(ctx, b.ScaleSetID, "runnerloom-"+m.Config.Name+"-"+p.Name)
	if e != nil {
		return errors.New("scale-set listener session failed")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	initial := session.Session()
	if initial.Statistics == nil {
		return errors.New("scale-set session statistics missing")
	}
	if e = m.Store.PersistMessage(ctx, initial.SessionID.String(), 0, p.Name, int64(initial.Statistics.TotalAssignedJobs), nil); e != nil {
		return e
	}
	last := 0
	for {
		msg, e := session.GetMessage(ctx, last, int(p.MaxRunners))
		if e != nil {
			return errors.New("scale-set polling interrupted")
		}
		if ctx.Err() != nil {
			return nil
		}
		if msg == nil {
			continue
		}
		if e = PersistThenAcknowledge(ctx, m.Store, session.Session().SessionID.String(), p, msg, session.DeleteMessage, session.AcquireJobs); e != nil {
			return errors.New("scale-set message processing failed; message not silently discarded")
		}
		last = msg.MessageID
	}
}

// Reconcile provisions only within the shared transactional resource ledger.
// GitHub can select a runner, but it cannot dictate arbitrary host operations.
func Reconcile(ctx context.Context, s *core.Store, p core.Pool, setID int, api JITAPI) error {
	demands, e := s.Demands(ctx)
	if e != nil {
		return e
	}
	var demand *core.Demand
	for _, d := range demands {
		if d.Pool == p.Name {
			v := d
			demand = &v
		}
	}
	if demand == nil || time.Since(demand.Seen) > 90*time.Second || demand.Blocked {
		return nil
	}
	runs, e := s.Instances(ctx)
	if e != nil {
		return e
	}
	count := int64(0)
	for _, a := range runs {
		if a.Pool.Name == p.Name && !a.Held.Empty() {
			count++
		}
	}
	target := min(p.MaxRunners, demand.Desired+p.WarmIdle)
	if p.Enabled && count < target {
		a, e := s.Allocate(ctx, core.ID(), p.Name)
		if e != nil {
			var fault *core.Error
			if errors.As(e, &fault) && fault.Code == "NO_CAPACITY" {
				return nil
			}
			return e
		}
		runs = append(runs, a)
	}
	for _, a := range runs {
		if a.Pool.Name != p.Name {
			continue
		}
		if a.State == "Reserved" && !a.JITReady {
			if time.Now().After(a.Deadline) {
				if e = s.StopInstance(ctx, a.ID); e != nil {
					return e
				}
				continue
			}
			old, e := api.GetRunnerByName(ctx, a.Name())
			if e != nil {
				return errors.New("cannot reconcile runner identity")
			}
			if old != nil {
				if old.RunnerScaleSetID != setID {
					return errors.New("runner name belongs to another scale set")
				}
				if e = api.RemoveRunner(ctx, int64(old.ID)); e != nil {
					return errors.New("cannot remove an undelivered JIT runner")
				}
			}
			jit, e := api.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: a.Name(), WorkFolder: "_work"}, setID)
			if e != nil || jit == nil || jit.Runner == nil {
				return errors.New("JIT runner creation failed")
			}
			if jit.Runner.Name != a.Name() {
				return errors.New("JIT identity mismatch")
			}
			if e = s.SetJIT(ctx, a.ID, int64(jit.Runner.ID), jit.EncodedJITConfig); e != nil {
				return e
			}
		}
	}
	return nil
}
func (m *Manager) Run(ctx context.Context) error {
	if e := m.Check(ctx); e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for _, p := range m.Config.Pools {
		if !p.Enabled {
			continue
		}
		b, e := m.bind(ctx, p)
		if e != nil {
			return e
		}
		m.Bindings[p.Name] = b
		wg.Add(1)
		go func(p core.Pool, b Binding) {
			defer wg.Done()
			delay := time.Second
			for ctx.Err() == nil {
				e := m.listen(ctx, p, b)
				if ctx.Err() != nil {
					return
				}
				if e != nil {
					m.Log.Warn("GitHub listener reconnecting", "pool", p.Name)
				}
				t := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				delay = min(delay*2, 30*time.Second)
			}
		}(p, b)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	refresh := time.Time{}
	policy := time.Time{}
	fingerprint := core.Fingerprint(m.Config)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			latest, _, e := m.Store.Config(ctx)
			if e != nil {
				return e
			}
			if core.Fingerprint(latest) != fingerprint {
				return core.Fail("CONFIG_RESTART_REQUIRED", "設定を変更しました。Controllerを再起動して接続を更新してください", nil)
			}
			if time.Since(policy) > time.Minute {
				if e = m.Check(ctx); e != nil {
					return e
				}
				policy = time.Now()
			}
			if time.Since(refresh) > 30*time.Second {
				for name, b := range m.Bindings {
					set, e := m.Client.GetRunnerScaleSetByID(ctx, b.ScaleSetID)
					if e == nil && set != nil && set.Statistics != nil {
						if e = m.Store.RefreshDemand(ctx, name, int64(set.Statistics.TotalAssignedJobs)); e != nil {
							return e
						}
					}
				}
				refresh = time.Now()
			}
			for _, p := range m.Config.Pools {
				b, ok := m.Bindings[p.Name]
				if !ok {
					continue
				}
				if e = Reconcile(ctx, m.Store, p, b.ScaleSetID, m.Client); e != nil {
					m.Log.Warn("runner provisioning deferred", "pool", p.Name, "error", e.Error())
				}
			}
		}
	}
}
