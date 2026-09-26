#!/usr/bin/env python3
from __future__ import annotations

from pathlib import Path

ROOT = Path.cwd()


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def write(path: str, content: str) -> None:
    target = ROOT / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count == 0 and new in text:
        return text
    if count != 1:
        raise SystemExit(f"{label}: expected one match, found {count}")
    return text.replace(old, new, 1)


def append_if_missing(path: str, heading: str, body: str) -> None:
    content = read(path)
    if heading in content:
        return
    write(path, content.rstrip() + "\n\n" + heading + "\n\n" + body.strip() + "\n")


def insert_before_first_h2(path: str, section: str) -> None:
    content = read(path)
    heading = section.strip().splitlines()[0]
    if heading in content:
        return
    marker = "\n## "
    index = content.find(marker)
    if index < 0:
        content = content.rstrip() + "\n\n" + section.strip() + "\n"
    else:
        content = content[: index + 1] + section.strip() + "\n\n" + content[index + 1 :]
    write(path, content)


cli = read("internal/cli/cli.go")
old_root = '\troot.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }\n'
new_root = (
    '\ta.addInteractive(root)\n'
    '\troot.RunE = func(c *cobra.Command, _ []string) error {\n'
    '\t\tif a.shouldStartInteractive() {\n'
    '\t\t\treturn a.runInteractive(c.Context())\n'
    '\t\t}\n'
    '\t\treturn c.Help()\n'
    '\t}\n'
)
if "a.addInteractive(root)" not in cli:
    cli = replace_once(cli, old_root, new_root, "root interactive integration")
write("internal/cli/cli.go", cli)

interactive = read("internal/cli/interactive.go")
if '\t"unicode"\n' not in interactive:
    interactive = replace_once(interactive, '\t"time"\n', '\t"time"\n\t"unicode"\n', "unicode import")

old_dispatch = (
    '\tif !containsStateFlag(args) {\n'
    '\t\tfull = append(full, "--state", a.State)\n'
    '\t}\n'
    '\tfull = append(full, args...)\n'
)
new_dispatch = (
    '\tif !containsStateFlag(args) {\n'
    '\t\tfull = append(full, "--state", a.State)\n'
    '\t}\n'
    '\tif !allowPrompts {\n'
    '\t\tfull = append(full, "--non-interactive")\n'
    '\t}\n'
    '\tfull = append(full, args...)\n'
)
if old_dispatch in interactive:
    interactive = interactive.replace(old_dispatch, new_dispatch, 1)
if new_dispatch not in interactive:
    raise SystemExit("nested command dispatch is not explicitly non-interactive")

for old, new in {
    'fmt.Fprintf(a.Err, "%s: ", label)': 'fmt.Fprintf(a.Err, "%s: ", interactiveSafe(label))',
    'fmt.Fprintf(a.Err, "%s [%s]: ", label, interactiveSafe(fallback))': 'fmt.Fprintf(a.Err, "%s [%s]: ", interactiveSafe(label), interactiveSafe(fallback))',
    'fmt.Fprintf(a.Err, "\\n%s\\n", title)': 'fmt.Fprintf(a.Err, "\\n%s\\n", interactiveSafe(title))',
    'fmt.Fprintf(a.Err, "  %s. %s\\n", item.Key, item.Label)': 'fmt.Fprintf(a.Err, "  %s. %s\\n", interactiveSafe(item.Key), interactiveSafe(item.Label))',
    'fmt.Fprintf(a.Err, "\\n%s\\n", summary)': 'fmt.Fprintf(a.Err, "\\n%s\\n", interactiveSafe(summary))',
}.items():
    interactive = interactive.replace(old, new)

old_path = 'if filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" {'
new_path = 'if filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && strings.IndexFunc(value, unicode.IsControl) == -1 {'
if old_path in interactive:
    interactive = interactive.replace(old_path, new_path, 1)
if new_path not in interactive:
    raise SystemExit("path control-character rejection is missing")

duration_marker = "func (a *App) interactiveDuration(label string, fallback time.Duration, minimum, maximum time.Duration) (time.Duration, error) {\n"
id_helper = '''func (a *App) interactiveID(label string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, "")
		if err != nil {
			return "", err
		}
		if core.ValidID(value) {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "32桁の小文字16進数IDを入力してください。")
	}
}

'''
if "func (a *App) interactiveID(" not in interactive:
    interactive = replace_once(interactive, duration_marker, id_helper + duration_marker, "interactive ID helper")
interactive = interactive.replace(
    'id, promptErr := a.interactiveRequired("承認する申請ID", "")',
    'id, promptErr := a.interactiveID("承認する申請ID")',
    1,
)

int_helper = '''func (a *App) interactiveInt64(label string, fallback, minimum, maximum int64) (int64, error) {
	for {
		value, err := a.interactiveRequired(label, strconv.FormatInt(fallback, 10))
		if err != nil {
			return 0, err
		}
		number, err := strconv.ParseInt(value, 10, 64)
		if err == nil && number >= minimum && number <= maximum {
			return number, nil
		}
		_, _ = fmt.Fprintf(a.Err, "%d〜%dの整数を入力してください。\n", minimum, maximum)
	}
}

'''
if "func (a *App) interactiveInt64(" not in interactive:
    interactive = replace_once(interactive, duration_marker, int_helper + duration_marker, "interactive integer helper")

old_limit = '''		value, err := a.interactiveRequired("Controller cache上限 GiB", "100")
		if err != nil {
			return nil, false, err
		}
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 1 {
			return nil, false, errors.New("cache上限は1以上の整数GiBで指定してください")
		}
'''
new_limit = '''		limit, err := a.interactiveInt64("Controller cache上限 GiB", 100, 1, 1048576)
		if err != nil {
			return nil, false, err
		}
'''
if old_limit in interactive:
    interactive = interactive.replace(old_limit, new_limit, 1)
if new_limit not in interactive:
    raise SystemExit("cache limit prompt is not range-validated")

old_cache_menu = '''			{"5", "同一HostのControllerからNodeへseed"},
			{"0", "戻る"},
'''
new_cache_menu = '''			{"5", "同一HostのControllerからNodeへseed"},
			{"6", "Golden Imageをbuild"},
			{"7", "qcow2をController cacheへimport"},
			{"0", "戻る"},
'''
if old_cache_menu in interactive:
    interactive = interactive.replace(old_cache_menu, new_cache_menu, 1)
if new_cache_menu not in interactive:
    raise SystemExit("Golden Image menu entries are missing")

cache_marker = '\t\tif choice == "5" {\n'
cache_handlers = '''		if choice == "6" {
			out, promptErr := a.interactiveAbsolutePath("Golden Image出力先", "/var/lib/runnerloom/builds/ubuntu-runner.qcow2")
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("Canonical署名と公式Runnerのdigestを検証して未登録qcow2をbuildします。既存出力は上書きしません。", "BUILD")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"image", "build", "--out", out}, false)
			}
			continue
		}
		if choice == "7" {
			file, promptErr := a.interactiveAbsolutePath("importする独立qcow2", "/var/lib/runnerloom/builds/ubuntu-runner.qcow2")
			if promptErr != nil {
				return promptErr
			}
			digest, promptErr := a.interactiveDigest("期待するImage digest")
			if promptErr != nil {
				return promptErr
			}
			limit, promptErr := a.interactiveInt64("Controller cache上限 GiB", 100, 1, 1048576)
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("Image全体のSHA-256とqcow2構造を検証してController cacheへatomicに登録します。", "IMPORT")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"image", "import", "--file", file, "--digest", digest, "--cache-gib", strconv.FormatInt(limit, 10)}, false)
			}
			continue
		}
'''
if "Golden Image出力先" not in interactive:
    interactive = replace_once(interactive, cache_marker, cache_handlers + cache_marker, "Golden Image handlers")

old_pool_menu = '''			{"3", "GitHub App・Runner Group・Repository権限を検査"},
			{"0", "戻る"},
'''
new_pool_menu = '''			{"3", "GitHub App・Runner Group・Repository権限を検査"},
			{"4", "Cluster設定JSONを検証"},
			{"5", "Cluster設定をplanして適用"},
			{"0", "戻る"},
'''
if old_pool_menu in interactive:
    interactive = interactive.replace(old_pool_menu, new_pool_menu, 1)
if new_pool_menu not in interactive:
    raise SystemExit("configuration menu entries are missing")

old_pool_case = '''		case "3":
			a.interactiveRun(ctx, []string{"github", "check"}, false)
		}
'''
new_pool_case = '''		case "3":
			a.interactiveRun(ctx, []string{"github", "check"}, false)
		case "4", "5":
			cwd, _ := os.Getwd()
			if cwd == "" {
				cwd = os.TempDir()
			}
			configPath, promptErr := a.interactiveAbsolutePath("Cluster設定JSON", filepath.Join(cwd, "cluster.json"))
			if promptErr != nil {
				return promptErr
			}
			if choice == "4" {
				a.interactiveRun(ctx, []string{"config", "validate", "--file", configPath}, false)
				continue
			}
			if !a.interactiveRun(ctx, []string{"config", "plan", "--file", configPath}, false) {
				continue
			}
			planID, promptErr := a.interactiveID("上に表示されたplan ID")
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("planは10分で失効し、競合時は拒否されます。ホストnetworkやserviceは変更しません。", "APPLY "+planID)
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"config", "apply", "--plan", planID}, false)
			}
		}
'''
if old_pool_case in interactive:
    interactive = interactive.replace(old_pool_case, new_pool_case, 1)
if new_pool_case not in interactive:
    raise SystemExit("configuration handlers are missing")

for marker in (
    "interactiveSafe(label)",
    "interactiveSafe(summary)",
    "func (a *App) interactiveID(",
    'a.interactiveID("承認する申請ID")',
    "func (a *App) interactiveInt64(",
    "Golden Image出力先",
    "Cluster設定をplanして適用",
):
    if marker not in interactive:
        raise SystemExit(f"missing interactive invariant: {marker}")
write("internal/cli/interactive.go", interactive)

tests = read("internal/cli/interactive_test.go")
if '\t"errors"\n' not in tests:
    tests = tests.replace('\t"context"\n', '\t"context"\n\t"errors"\n', 1)
tests = tests.replace(
    'if err == nil || err.Error() != "boom" {',
    'if !errors.Is(err, io.ErrUnexpectedEOF) {',
    1,
)
if "!errors.Is(err, io.ErrUnexpectedEOF)" not in tests:
    raise SystemExit("reader failure test is incorrect")
write("internal/cli/interactive_test.go", tests)

setup = read("internal/cli/setup.go")
setup = setup.replace(
    'fmt.Fprintf(a.Err, "%s [%s]: ", label, fallback)',
    'fmt.Fprintf(a.Err, "%s [%s]: ", interactiveSafe(label), interactiveSafe(fallback))',
    1,
)
if "interactiveSafe(label), interactiveSafe(fallback)" not in setup:
    raise SystemExit("legacy setup prompt is not sanitized")
write("internal/cli/setup.go", setup)

write(
    "internal/cli/interactive_quality_test.go",
    r'''package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractiveAbsolutePathRejectsControls(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("/tmp/bad\x1b[2J\n/tmp/good path\n"), Out: &out, Err: &errOut}
	got, err := a.interactiveAbsolutePath("path", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/good path" {
		t.Fatalf("got %q", got)
	}
}

func TestInteractiveInt64Reprompts(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("invalid\n0\n128\n"), Out: &out, Err: &errOut}
	got, err := a.interactiveInt64("cache", 100, 1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got != 128 {
		t.Fatalf("got %d", got)
	}
}
''',
)

insert_before_first_h2(
    "README.md",
    '''
## 対話CLI

端末で引数なしの`runnerloom`を実行すると、人向けの対話メニューを開きます。既存のサブコマンドを内部で再利用するため、通常CLIと処理・安全条件は共通です。

```bash
runnerloom
# または
runnerloom interactive
```

パイプ、リダイレクト、CI、`--json`、`--non-interactive`、明示したサブコマンドでは入力待ちせず、従来の非対話動作を維持します。詳しくは[対話CLIガイド](docs/INTERACTIVE_CLI.ja.md)を参照してください。
''',
)
append_if_missing(
    "docs/README.md",
    "## 対話メニューから操作する",
    "- [対話CLI](INTERACTIVE_CLI.ja.md) — TTY自動判定、メニュー、非対話互換性、確認語、安全境界",
)
append_if_missing(
    "docs/OPERATIONS.ja.md",
    "## 対話メニューから運用する",
    "`runnerloom`または`runnerloom interactive`から状態、Image/Cache、network、Node、設定、Pool/GitHub、backup、maintenance、serviceを選べます。自動化では従来のサブコマンドと`--non-interactive`を使用してください。",
)
if (ROOT / "docs/CACHE.ja.md").is_file():
    append_if_missing(
        "docs/CACHE.ja.md",
        "## 対話メニューからCacheを操作する",
        "Image cacheメニューからstatus、完全検証、prune dry-run/apply、same-host seed、Golden Image build、Controller importを実行できます。`prune --apply`は先にdry-runを表示し、完全一致の確認語を要求します。",
    )
append_if_missing(
    "SECURITY.md",
    "## Interactive CLI boundary",
    "Interactive mode requires terminal-connected stdin, stdout, and stderr. JSON, non-interactive, piped, redirected, service, and explicit-command invocations do not prompt. Values are passed as argument slices to existing commands, dynamic terminal text is sanitized, and destructive operations retain exact confirmations and existing fail-closed checks.",
)

quick = read("docs/QUICKSTART.ja.md")
if "INTERACTIVE_CLI.ja.md" not in quick:
    marker = "## このガイドの完了条件"
    note = "> [!TIP]\n> 端末で`runnerloom`だけを実行すると対話メニューを利用できます。詳細は[対話CLI](INTERACTIVE_CLI.ja.md)を参照してください。このQuick Startの完了条件自体は省略されません。\n\n"
    if marker not in quick:
        raise SystemExit("Quick Start insertion marker is missing")
    quick = quick.replace(marker, note + marker, 1)
    write("docs/QUICKSTART.ja.md", quick)

checker = read("scripts/check-docs.py")
entry = '    ROOT / "docs" / "INTERACTIVE_CLI.ja.md",\n'
if entry not in checker:
    for marker in (
        '    ROOT / "docs" / "CACHE.ja.md",\n',
        '    ROOT / "docs" / "TROUBLESHOOTING.ja.md",\n',
        '    ROOT / "docs" / "OPERATIONS.ja.md",\n',
    ):
        if marker in checker:
            checker = checker.replace(marker, marker + entry, 1)
            break
    else:
        raise SystemExit("documentation checker insertion marker is missing")
    write("scripts/check-docs.py", checker)

write("VERSION", "0.1.0-rc.4\n")
changelog = read("CHANGELOG.md")
if "## 0.1.0-rc.4" not in changelog:
    entry_text = '''## 0.1.0-rc.4

- Added a guarded interactive operator menu for setup, diagnostics, Golden Images, image cache, VM network, Node, configuration, Pool/GitHub, backup/maintenance, and systemd service workflows.
- Preserved explicit subcommands and machine-readable behavior; non-TTY, `--json`, and `--non-interactive` invocations never wait for menu input.
- Added exact confirmation phrases, dry-run-first destructive paths, terminal sanitization, bounded input, argument-array dispatch, and regression coverage.

'''
    index = changelog.find("\n## ")
    changelog = changelog.rstrip() + "\n\n" + entry_text if index < 0 else changelog[: index + 1] + entry_text + changelog[index + 1 :]
    write("CHANGELOG.md", changelog)

for path in (ROOT / ".github/workflows").glob("internal-*interactive*.yml"):
    path.unlink()
for name in (
    ".github/workflows/internal-unblock-interactive-rc4.yml",
    "scripts/apply-interactive-rc4.py",
):
    (ROOT / name).unlink(missing_ok=True)
