#!/usr/bin/env python3
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def write(path: str, content: str) -> None:
    (ROOT / path).write_text(content, encoding="utf-8")


def replace_once(path: str, old: str, new: str) -> None:
    content = read(path)
    count = content.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected one match, found {count}")
    write(path, content.replace(old, new, 1))


def insert_before_first_h2(path: str, section: str) -> None:
    content = read(path)
    if section.splitlines()[0] in content:
        return
    marker = "\n## "
    index = content.find(marker)
    if index < 0:
        write(path, content.rstrip() + "\n\n" + section.strip() + "\n")
        return
    write(path, content[: index + 1] + section.strip() + "\n\n" + content[index + 1 :])


def append_section(path: str, heading: str, body: str) -> None:
    content = read(path)
    if heading in content:
        return
    write(path, content.rstrip() + "\n\n" + heading + "\n\n" + body.strip() + "\n")


replace_once(
    "internal/cli/cli.go",
    '\troot.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }\n',
    '\ta.addInteractive(root)\n'
    '\troot.RunE = func(c *cobra.Command, _ []string) error {\n'
    '\t\tif a.shouldStartInteractive() {\n'
    '\t\t\treturn a.runInteractive(c.Context())\n'
    '\t\t}\n'
    '\t\treturn c.Help()\n'
    '\t}\n',
)

insert_before_first_h2(
    "README.md",
    """
## 対話CLI

端末で引数なしの`runnerloom`を実行すると、人向けの対話メニューを開きます。既存のサブコマンドを内部で再利用するため、通常CLIと処理・安全条件は共通です。

```bash
runnerloom
# または
runnerloom interactive
```

パイプ、リダイレクト、CI、`--json`、`--non-interactive`、明示したサブコマンドでは入力待ちせず、従来の非対話動作を維持します。Cache pruneやnetwork applyなどは、dry-runまたは計画を表示し、完全一致の確認語を入力した場合だけ適用します。

詳しくは[対話CLIガイド](docs/INTERACTIVE_CLI.ja.md)を参照してください。
""",
)

append_section(
    "docs/README.md",
    "## 対話メニューから操作する",
    """
- [対話CLI](INTERACTIVE_CLI.ja.md) — 引数なし起動、メニュー構成、非TTY/JSON互換性、確認語、安全境界

対話メニューは既存サブコマンドの代替APIではありません。自動化や再現可能な運用では、従来どおりサブコマンドと`--json` / `--non-interactive`を使用してください。
""",
)

quickstart = read("docs/QUICKSTART.ja.md")
if "INTERACTIVE_CLI.ja.md" not in quickstart:
    marker = "## このガイドの完了条件"
    note = """
> [!TIP]
> 端末で`runnerloom`だけを実行すると対話メニューを利用できます。初期設定、診断、Cache、network、Node、backup、serviceを番号から選べます。対話モードの安全条件は[対話CLI](INTERACTIVE_CLI.ja.md)を参照してください。このQuick Startの完了条件自体は省略されません。

"""
    if marker not in quickstart:
        raise SystemExit("docs/QUICKSTART.ja.md: insertion marker missing")
    write("docs/QUICKSTART.ja.md", quickstart.replace(marker, note + marker, 1))

append_section(
    "docs/OPERATIONS.ja.md",
    "## 対話メニューから運用する",
    """
端末からの日常操作は`runnerloom`または`runnerloom interactive`でも実行できます。状態・Node・Pool・GitHub・Cache・network・backup・maintenance・serviceをメニューから選択できます。

```bash
sudo runnerloom --state /var/lib/runnerloom/controller interactive
```

Cache pruneとDB compactは、対話側でも最初にdry-runを表示します。変更を適用するには、候補と停止条件を確認した後に指定された大文字の確認語を完全一致で入力します。systemdや自動化では対話モードを使わず、従来のサブコマンドと`--non-interactive`を使用してください。詳細は[対話CLI](INTERACTIVE_CLI.ja.md)を参照してください。
""",
)

cache_path = ROOT / "docs" / "CACHE.ja.md"
if cache_path.exists():
    append_section(
        "docs/CACHE.ja.md",
        "## 対話メニューからCacheを操作する",
        """
`runnerloom`の「Image cache」メニューから、Controller/Nodeのstatus、完全検証、prune dry-run/apply、same-host seedを実行できます。`prune --apply`の前には必ず同じ条件のdry-runを表示し、`PRUNE`の完全一致確認を要求します。対象serviceの停止、所有権、VM参照、hard link、qcow2 backing、digestのfail-closed検査は通常CLIと同じです。

自動化では対話メニューを使わず、`runnerloom cache ... --json --non-interactive`を使用してください。
""",
    )

append_section(
    "SECURITY.md",
    "## Interactive CLI boundary",
    """
The interactive menu is enabled automatically only when stdin, stdout, and stderr are terminals and neither `--json` nor `--non-interactive` is active. Redirected, piped, service, and CI invocations retain non-interactive behavior.

Menu values are passed to existing Cobra commands as argument slices; they are not concatenated into a shell command. Destructive or host-changing paths require an exact confirmation phrase, and cache pruning and database compaction show a dry run first. Existing process locks, ownership checks, digest verification, VM-state checks, and fail-closed behavior remain authoritative. The menu never asks for or prints invitation secrets, JIT configuration, private-key contents, or PAT contents.
""",
)

checker = read("scripts/check-docs.py")
interactive_doc = '    ROOT / "docs" / "INTERACTIVE_CLI.ja.md",\n'
if interactive_doc not in checker:
    candidates = [
        '    ROOT / "docs" / "CACHE.ja.md",\n',
        '    ROOT / "docs" / "TROUBLESHOOTING.ja.md",\n',
        '    ROOT / "docs" / "OPERATIONS.ja.md",\n',
    ]
    for marker in candidates:
        if marker in checker:
            checker = checker.replace(marker, marker + interactive_doc, 1)
            break
    else:
        raise SystemExit("scripts/check-docs.py: DOCS insertion marker missing")
    write("scripts/check-docs.py", checker)

write("VERSION", "0.1.0-rc.4\n")

changelog = read("CHANGELOG.md")
release_heading = "## 0.1.0-rc.4"
if release_heading not in changelog:
    entry = """
## 0.1.0-rc.4

- Added an interactive operator menu for setup, diagnostics, cache, network, Node, Pool/GitHub, backup/maintenance, and systemd service workflows.
- Preserved all existing subcommands and machine-readable behavior; non-TTY, `--json`, `--non-interactive`, and explicit-command invocations never wait for menu input.
- Added exact confirmation phrases and dry-run-first handling for destructive or host-changing operations.
- Added bounded input handling, argument-array dispatch without shell evaluation, EOF-safe exit behavior, documentation, and regression tests.

"""
    first_entry = changelog.find("\n## ")
    if first_entry < 0:
        changelog = changelog.rstrip() + "\n\n" + entry
    else:
        changelog = changelog[: first_entry + 1] + entry + changelog[first_entry + 1 :]
    write("CHANGELOG.md", changelog)

for temporary in (
    ROOT / ".github" / "workflows" / "internal-apply-interactive-rc4.yml",
    ROOT / "scripts" / "apply-interactive-rc4.py",
):
    temporary.unlink(missing_ok=True)
