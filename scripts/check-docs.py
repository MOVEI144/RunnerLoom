#!/usr/bin/env python3
"""Validate the reader-facing RunnerLoom documentation without network access."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
DOCS = (
    ROOT / "README.md",
    ROOT / "docs" / "README.md",
    ROOT / "docs" / "INSTALL.md",
    ROOT / "docs" / "QUICKSTART.ja.md",
    ROOT / "docs" / "MULTI_NODE.ja.md",
    ROOT / "docs" / "OPERATIONS.ja.md",
    ROOT / "docs" / "TROUBLESHOOTING.ja.md",
)
LINK = re.compile(r"(?<!!)\[[^\]]+\]\(([^)]+)\)")
VERSION = re.compile(r"(?<![A-Za-z0-9])v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?")


def fail(message: str, errors: list[str]) -> None:
    errors.append(message)


def prose_without_fences(lines: list[str], relative: Path, errors: list[str]) -> str:
    prose: list[str] = []
    fence: str | None = None
    for line in lines:
        stripped = line.lstrip()
        marker = next((candidate for candidate in ("```", "~~~") if stripped.startswith(candidate)), None)
        if marker is not None:
            if fence is None:
                fence = marker
            elif marker == fence:
                fence = None
            continue
        if fence is None:
            prose.append(line)
    if fence is not None:
        fail(f"{relative}: unbalanced fenced code block", errors)
    return "\n".join(prose)


def local_target(source: Path, raw: str) -> Path | None:
    target = raw.strip()
    if target.startswith("<") and target.endswith(">"):
        target = target[1:-1]
    if not target or target.startswith("#"):
        return None
    parsed = urlsplit(target)
    if parsed.scheme or parsed.netloc:
        return None
    path = unquote(parsed.path)
    if not path:
        return None
    return (source.parent / path).resolve()


def validate_document(path: Path, errors: list[str]) -> str:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        fail(f"{path.relative_to(ROOT)}: cannot read: {exc}", errors)
        return ""

    relative = path.relative_to(ROOT)
    prose = prose_without_fences(text.splitlines(), relative, errors)
    lines = prose.splitlines()
    first = next((line for line in lines if line.strip()), "")
    if not first.startswith("# "):
        fail(f"{relative}: first non-empty prose line must be one H1 heading", errors)
    if sum(line.startswith("# ") for line in lines) != 1:
        fail(f"{relative}: expected exactly one H1 heading outside code fences", errors)

    for match in LINK.finditer(prose):
        target = local_target(path, match.group(1))
        if target is None:
            continue
        try:
            target.relative_to(ROOT)
        except ValueError:
            fail(f"{relative}: local link escapes repository: {match.group(1)}", errors)
            continue
        if not target.exists():
            fail(f"{relative}: broken local link: {match.group(1)}", errors)

    return text


def require(text: str, path: Path, values: tuple[str, ...], errors: list[str]) -> None:
    relative = path.relative_to(ROOT)
    for value in values:
        if value not in text:
            fail(f"{relative}: required reader checkpoint missing: {value}", errors)


def main() -> int:
    errors: list[str] = []
    texts = {path: validate_document(path, errors) for path in DOCS}

    readme = texts[ROOT / "README.md"]
    require(
        readme,
        ROOT / "README.md",
        (
            "docs/README.md",
            "docs/QUICKSTART.ja.md",
            "docs/TROUBLESHOOTING.ja.md",
            "setup --apply",
            "どの操作がホストを変更するか",
        ),
        errors,
    )

    quickstart_path = ROOT / "docs" / "QUICKSTART.ja.md"
    quickstart = texts[quickstart_path]
    require(
        quickstart,
        quickstart_path,
        (
            "このガイドの完了条件",
            "setupが行うこと",
            "setupが行わないこと",
            "runnerloom doctor --strict",
            "runnerloom github check",
            "runnerloom network plan",
            "runnerloom network apply",
            "runnerloom network check",
            "runnerloom service install",
            "runnerloom vm list",
            "Job後の確認",
        ),
        errors,
    )

    install_path = ROOT / "docs" / "INSTALL.md"
    install = texts[install_path]
    if VERSION.search(install):
        fail(f"{install_path.relative_to(ROOT)}: installation guide must not pin a release number", errors)
    require(
        install,
        install_path,
        (
            "sha256sum --check",
            "command -v runnerloom",
            "backup --out",
            "既存ファイルを上書きしません",
        ),
        errors,
    )

    combined = "\n".join(texts.values())
    forbidden = {
        'findmnt -T "$RL_NODE_STATE/images"': "image cache may not exist before first Agent start",
        "--out /var/backups/runnerloom/controller.db": "backup destinations must be new files",
    }
    for value, reason in forbidden.items():
        if value in combined:
            fail(f"reader docs contain unsafe copy/paste command {value!r}: {reason}", errors)

    if errors:
        print("Documentation validation failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    print(f"Validated {len(DOCS)} reader-facing documents and their local links.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
