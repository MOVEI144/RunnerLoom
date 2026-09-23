package tasks

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

// FileSpec names one local credential/config file. Path is where it is placed
// below the guest home; it is also read from below the local home unless From
// names another local location (absolute or starting with "~/").
type FileSpec struct {
	Path     string `json:"path"`
	From     string `json:"from,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// Profile describes one coding-agent CLI.
type Profile struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Binary      string     `json:"binary,omitempty"`
	Runtime     string     `json:"runtime,omitempty"`
	Install     string     `json:"install,omitempty"`
	Run         []string   `json:"run"`
	Files       []FileSpec `json:"files,omitempty"`
	Env         []string   `json:"env,omitempty"`
	Builtin     bool       `json:"-"`
}

func (p Profile) Agent() core.AgentProfile {
	return core.AgentProfile{Name: p.Name, Binary: p.Binary, Runtime: p.Runtime, Install: p.Install, Run: p.Run}
}

func (p Profile) Validate() error {
	if e := p.Agent().Validate(); e != nil {
		return e
	}
	for _, f := range p.Files {
		if !core.ValidHomePath(f.Path) || (f.From != "" && !filepath.IsAbs(f.From) && !strings.HasPrefix(f.From, "~/")) {
			return core.Fail("INVALID_AGENT", "filesのpathはホーム配下の相対パス、fromは絶対パスか~/です", p.Name)
		}
	}
	for _, k := range p.Env {
		if !validEnvName(k) {
			return core.Fail("INVALID_AGENT", "envに使えない環境変数名があります", p.Name+":"+k)
		}
	}
	return nil
}

func validEnvName(k string) bool {
	spec := core.TaskSpec{RequestID: "r", Pool: "p", Agent: core.AgentProfile{Name: "a", Run: []string{"a"}}, Prompt: "p", Env: map[string]string{k: ""}}
	return spec.Validate() == nil
}

// Builtins are profiles for CLIs whose headless mode and credential location
// are documented publicly. They are not qualified inside a real VM by this
// repository's tests; override any of them in agents.json.
func Builtins() []Profile {
	return []Profile{
		{Name: "claude", Description: "Claude Code (claude -p). OAuth file or CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY", Binary: "claude", Runtime: "node", Install: "npm install -g @anthropic-ai/claude-code",
			Run:   []string{"claude", "-p", "{prompt}", "--dangerously-skip-permissions"},
			Files: []FileSpec{{Path: ".claude/.credentials.json", Optional: true}}, Env: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}},
		{Name: "codex", Description: "OpenAI Codex CLI (codex exec). ~/.codex/auth.json or OPENAI_API_KEY", Binary: "codex", Runtime: "node", Install: "npm install -g @openai/codex",
			Run:   []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "{prompt}"},
			Files: []FileSpec{{Path: ".codex/auth.json", Optional: true}}, Env: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}},
		{Name: "opencode", Description: "opencode (opencode run). ~/.local/share/opencode/auth.json or provider API keys", Binary: "opencode", Runtime: "node", Install: "npm install -g opencode-ai",
			Run:   []string{"opencode", "run", "{prompt}"},
			Files: []FileSpec{{Path: ".local/share/opencode/auth.json", Optional: true}}, Env: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY"}},
		{Name: "pi", Description: "pi coding agent (pi -p). ~/.pi/agent/auth.json or provider API keys", Binary: "pi", Runtime: "node", Install: "npm install -g @mariozechner/pi-coding-agent",
			Run:   []string{"pi", "-p", "{prompt}"},
			Files: []FileSpec{{Path: ".pi/agent/auth.json", Optional: true}, {Path: ".pi/agent/settings.json", Optional: true}, {Path: ".pi/agent/models.json", Optional: true}}, Env: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"}},
		{Name: "gemini", Description: "Gemini CLI (gemini -p). ~/.gemini OAuth files or GEMINI_API_KEY", Binary: "gemini", Runtime: "node", Install: "npm install -g @google/gemini-cli",
			Run:   []string{"gemini", "--yolo", "-p", "{prompt}"},
			Files: []FileSpec{{Path: ".gemini/oauth_creds.json", Optional: true}, {Path: ".gemini/google_accounts.json", Optional: true}, {Path: ".gemini/settings.json", Optional: true}}, Env: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	}
}

type agentsFile struct {
	Agents []Profile `json:"agents"`
}

// LoadProfiles returns built-ins overlaid by <dir>/agents.json, which may add
// agents (for example grok, musecode or agy) or replace a built-in by name.
func LoadProfiles(dir string) (map[string]Profile, error) {
	out := map[string]Profile{}
	for _, p := range Builtins() {
		p.Builtin = true
		out[p.Name] = p
	}
	path := filepath.Join(dir, "agents.json")
	b, e := readLocal(path, core.MaxJSON)
	if os.IsNotExist(e) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	if st, e := os.Stat(path); e == nil && st.Mode().Perm()&0022 != 0 {
		return nil, core.Fail("UNSAFE_AGENTS_FILE", "agents.jsonを他ユーザーが書き換えられます。chmod go-w してください", path)
	}
	var f agentsFile
	if e = core.Decode(bytes.NewReader(b), &f); e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, p := range f.Agents {
		if e = p.Validate(); e != nil {
			return nil, e
		}
		if seen[p.Name] {
			return nil, core.Fail("INVALID_AGENT", "agents.jsonに同じ名前があります", p.Name)
		}
		seen[p.Name] = true
		out[p.Name] = p
	}
	return out, nil
}

func SortedProfiles(m map[string]Profile) []Profile {
	out := make([]Profile, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readLocal reads a regular file without following a final symlink.
func readLocal(path string, limit int64) ([]byte, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		if errors.Is(e, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		if errors.Is(e, syscall.ELOOP) {
			return nil, core.Fail("UNSAFE_CREDENTIAL", "シンボリックリンクの認証ファイルは送信しません", path)
		}
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, core.Fail("UNSAFE_CREDENTIAL", "通常ファイルではありません", path)
	}
	if st.Size() > limit {
		return nil, core.Fail("CREDENTIAL_TOO_LARGE", "ファイルが大きすぎます", path)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

func (p Profile) source(home string, f FileSpec) string {
	switch {
	case f.From == "":
		return filepath.Join(home, filepath.FromSlash(f.Path))
	case strings.HasPrefix(f.From, "~/"):
		return filepath.Join(home, f.From[2:])
	default:
		return f.From
	}
}

// Credentials reports what would be sent, without contents.
type Credentials struct {
	Files []string `json:"files"`
	Env   []string `json:"env"`
}

// Collect reads the profile's local credentials. At least one file or
// environment variable must exist when the profile declares any.
func (p Profile) Collect(home string, getenv func(string) string) ([]core.TaskFile, map[string]string, Credentials, error) {
	files := []core.TaskFile{}
	env := map[string]string{}
	found := Credentials{Files: []string{}, Env: []string{}}
	for _, f := range p.Files {
		src := p.source(home, f)
		b, e := readLocal(src, core.MaxTaskFile)
		if errors.Is(e, os.ErrNotExist) {
			if f.Optional {
				continue
			}
			return nil, nil, found, core.Fail("CREDENTIALS_MISSING", "必須の認証ファイルがありません", src)
		}
		if e != nil {
			return nil, nil, found, e
		}
		files = append(files, core.TaskFile{Path: f.Path, Content: b})
		found.Files = append(found.Files, f.Path)
	}
	for _, k := range p.Env {
		if v := getenv(k); v != "" {
			env[k] = v
			found.Env = append(found.Env, k)
		}
	}
	if (len(p.Files) > 0 || len(p.Env) > 0) && len(files) == 0 && len(env) == 0 {
		looked := []string{}
		for _, f := range p.Files {
			looked = append(looked, p.source(home, f))
		}
		looked = append(looked, p.Env...)
		return nil, nil, found, core.Fail("CREDENTIALS_MISSING", p.Name+"の認証情報がこのPCで見つかりません。先にこのPCでログインするか、環境変数を設定してください", looked)
	}
	return files, env, found, nil
}

// GitHubToken reads GH_TOKEN / GITHUB_TOKEN, else <dir>/github-token (0600).
func GitHubToken(dir string, getenv func(string) string) (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v, nil
		}
	}
	b, e := core.ReadSecret(filepath.Join(dir, "github-token"))
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(b)), nil
}
