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
	// Sudo lets the agent use sudo in the VM. It can then read every secret
	// of the task, including the GitHub token.
	Sudo    bool `json:"sudo,omitempty"`
	Builtin bool `json:"-"`
}

func (p Profile) Agent() core.AgentProfile {
	return core.AgentProfile{Name: p.Name, Binary: p.Binary, Runtime: p.Runtime, Install: p.Install, Run: p.Run, Sudo: p.Sudo}
}

// Digest identifies the exact definition that the user allowed.
func (p Profile) Digest() string {
	p.Builtin = false
	return core.Fingerprint(p)
}

// sensitive local locations are never read as agent credentials, even when a
// profile asks for them (for example through a pasted agents.json).
var sensitivePrefixes = []string{".ssh/", ".gnupg/", ".aws/", ".kube/", ".docker/", ".config/gh/", ".password-store/", ".config/runnerloom/"}
var sensitiveFiles = []string{".netrc", ".git-credentials", ".pgpass", ".bash_history", ".zsh_history"}

func sensitive(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, f := range sensitiveFiles {
		if rel == f {
			return true
		}
	}
	for _, p := range sensitivePrefixes {
		if strings.HasPrefix(rel+"/", p) {
			return true
		}
	}
	return false
}

func (p Profile) Validate() error {
	if e := p.Agent().Validate(); e != nil {
		return e
	}
	for _, f := range p.Files {
		if !core.ValidHomePath(f.Path) || (f.From != "" && !filepath.IsAbs(f.From) && !strings.HasPrefix(f.From, "~/")) {
			return core.Fail("INVALID_AGENT", "filesのpathはホーム配下の相対パス、fromは絶対パスか~/です", p.Name)
		}
		if sensitive(f.Path) || (strings.HasPrefix(f.From, "~/") && sensitive(f.From[2:])) {
			return core.Fail("INVALID_AGENT", "SSH鍵・クラウド認証・履歴などの場所は送信できません", p.Name+":"+f.Path)
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
// are documented publicly (checked against each project's documentation and
// sources in 2026-09). They are not qualified inside a real VM by this
// repository's tests; override any of them in agents.json.
func Builtins() []Profile {
	return []Profile{
		{Name: "claude", Description: "Claude Code (claude -p). ~/.claude/.credentials.json, CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY (the API key wins when both are set)", Binary: "claude", Runtime: "node", Install: "npm install -g @anthropic-ai/claude-code",
			Run:   []string{"claude", "-p", "{prompt}", "--dangerously-skip-permissions"},
			Files: []FileSpec{{Path: ".claude/.credentials.json", Optional: true}}, Env: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}},
		{Name: "codex", Description: "OpenAI Codex CLI (codex exec). ~/.codex/auth.json or CODEX_API_KEY", Binary: "codex", Runtime: "node", Install: "npm install -g @openai/codex",
			Run:   []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "{prompt}"},
			Files: []FileSpec{{Path: ".codex/auth.json", Optional: true}}, Env: []string{"CODEX_API_KEY"}},
		{Name: "opencode", Description: "opencode (opencode run --auto). ~/.local/share/opencode/auth.json or provider API keys", Binary: "opencode", Runtime: "node", Install: "npm install -g opencode-ai",
			Run:   []string{"opencode", "run", "--auto", "{prompt}"},
			Files: []FileSpec{{Path: ".local/share/opencode/auth.json", Optional: true}, {Path: ".config/opencode/opencode.json", Optional: true}}, Env: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY"}},
		{Name: "pi", Description: "pi coding agent (pi -p). ~/.pi/agent/auth.json or provider API keys", Binary: "pi", Runtime: "node", Install: "npm install -g --ignore-scripts @earendil-works/pi-coding-agent",
			Run:   []string{"pi", "-p", "{prompt}"},
			Files: []FileSpec{{Path: ".pi/agent/auth.json", Optional: true}, {Path: ".pi/agent/settings.json", Optional: true}, {Path: ".pi/agent/models.json", Optional: true}}, Env: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"}},
		{Name: "gemini", Description: "Gemini CLI (gemini -p). ~/.gemini OAuth files (settings.json must select the auth type) or GEMINI_API_KEY", Binary: "gemini", Runtime: "node", Install: "npm install -g @google/gemini-cli",
			Run:   []string{"gemini", "--yolo", "--skip-trust", "-p", "{prompt}"},
			Files: []FileSpec{{Path: ".gemini/oauth_creds.json", Optional: true}, {Path: ".gemini/google_accounts.json", Optional: true}, {Path: ".gemini/settings.json", Optional: true}}, Env: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"}},
		{Name: "grok", Description: "xAI Grok Build (grok --prompt-file). ~/.grok/auth.json or XAI_API_KEY", Binary: "grok",
			Install: `curl -fsSL https://x.ai/cli/install.sh | GROK_BIN_DIR=/usr/local/bin bash && cp --remove-destination "$(readlink -f /usr/local/bin/grok)" /usr/local/bin/grok`,
			Run:     []string{"grok", "--always-approve", "--no-auto-update", "--prompt-file", "{promptFile}"},
			Files:   []FileSpec{{Path: ".grok/auth.json", Optional: true}, {Path: ".grok/config.toml", Optional: true}}, Env: []string{"XAI_API_KEY"}},
		{Name: "musecode", Description: "Meta Muse Code (muse exec). ~/.config/muse/auth.json or META_API_KEY", Binary: "muse",
			Install: "curl -fsSL https://dev.meta.ai/install.sh | MUSE_INSTALL_DIR=/usr/local/bin bash",
			Run:     []string{"muse", "exec", "--yolo", "--prompt-file", "{promptFile}"},
			Files:   []FileSpec{{Path: ".config/muse/auth.json", Optional: true}, {Path: ".config/muse/settings.json", Optional: true}}, Env: []string{"META_API_KEY"}},
		{Name: "agy", Description: "Google Antigravity CLI (agy -p). GEMINI_API_KEY only: its account login lives in the OS keyring and cannot be copied", Binary: "agy",
			Install: "curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir /usr/local/bin",
			Run:     []string{"agy", "-p", "{prompt}", "--dangerously-skip-permissions"},
			Env:     []string{"GEMINI_API_KEY"}},
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
	Files   []string `json:"files"`
	Sources []string `json:"localSources"`
	Env     []string `json:"env"`
}

// Collect reads the profile's local credentials. At least one file or
// environment variable must exist when the profile declares any.
func (p Profile) Collect(home string, getenv func(string) string) ([]core.TaskFile, map[string]string, Credentials, error) {
	files := []core.TaskFile{}
	env := map[string]string{}
	found := Credentials{Files: []string{}, Sources: []string{}, Env: []string{}}
	for _, f := range p.Files {
		src := p.source(home, f)
		rel, e := filepath.Rel(home, src)
		if e != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) || sensitive(rel) {
			return nil, nil, found, core.Fail("UNSAFE_CREDENTIAL", "ホーム外や機密の場所（SSH鍵など）からは送信しません", src)
		}
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
		found.Sources = append(found.Sources, src)
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

// GitHubToken prefers the dedicated owner-only file <dir>/github-token (meant
// for a fine-grained PAT limited to the target repositories), then
// RUNNERLOOM_GITHUB_TOKEN, and only then the general GH_TOKEN / GITHUB_TOKEN.
func GitHubToken(dir string, getenv func(string) string) (string, error) {
	b, e := core.ReadSecret(filepath.Join(dir, "github-token"))
	if e == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	for _, k := range []string{"RUNNERLOOM_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v, nil
		}
	}
	return "", nil
}
