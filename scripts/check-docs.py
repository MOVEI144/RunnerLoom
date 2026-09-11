#!/usr/bin/env python3
"""Validate the reader-facing RunnerLoom documentation without network access."""

from __future__ import annotations

import html
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
DOCS = (
    ROOT / "README.md",
    ROOT / "docs" / "README.md",
    ROOT / "docs" / "INSTALL.md",
    ROOT / "docs" / "CACHE.ja.md",
    ROOT / "docs" / "QUICKSTART.ja.md",
    ROOT / "docs" / "MULTI_NODE.ja.md",
    ROOT / "docs" / "OPERATIONS.ja.md",
    ROOT / "docs" / "TROUBLESHOOTING.ja.md",
)
LINK = re.compile(r"(?<!!)\[[^\]]+\]\(([^)]+)\)")
FENCE = re.compile(r"^[ \t]{0,3}(`{3,}|~{3,})")
HEADING = re.compile(r"^#{1,6}[ \t]+(.+?)\s*$")
EXPLICIT_ANCHOR = re.compile(
    r"<[^>]+\b(?:id|name)\s*=\s*['\"]([^'\"]+)['\"][^>]*>", re.IGNORECASE
)
MARKDOWN_LINK = re.compile(r"!?\[([^\]]*)\]\([^)]*\)")
VERSION = re.compile(
    r"(?<![A-Za-z0-9.])v?\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:\.[0-9A-Za-z]+)*)?"
    r"(?![A-Za-z0-9.])"
)


@dataclass(frozen=True)
class LocalTarget:
    path: Path
    fragment: str | None


def fail(message: str, errors: list[str]) -> None:
    errors.append(message)


def prose_without_fences(lines: list[str], relative: Path, errors: list[str]) -> str:
    prose: list[str] = []
    fence: tuple[str, int] | None = None
    for line in lines:
        match = FENCE.match(line)
        if match is not None:
            token = match.group(1)
            marker = (token[0], len(token))
            if fence is None:
                fence = marker
            elif (
                marker[0] == fence[0]
                and marker[1] >= fence[1]
                and not line[match.end() :].strip()
            ):
                fence = None
            continue
        if fence is None:
            prose.append(line)
    if fence is not None:
        fail(f"{relative}: unbalanced fenced code block", errors)
    return "\n".join(prose)


def local_target(source: Path, raw: str) -> LocalTarget | None:
    target = raw.strip()
    if target.startswith("<") and target.endswith(">"):
        target = target[1:-1]
    if not target:
        return None
    parsed = urlsplit(target)
    if parsed.scheme or parsed.netloc:
        return None
    path = (source.parent / unquote(parsed.path)).resolve() if parsed.path else source.resolve()
    fragment = unquote(parsed.fragment) if parsed.fragment else None
    return LocalTarget(path=path, fragment=fragment)


def heading_text(raw: str) -> str:
    text = re.sub(r"[ \t]+#+[ \t]*$", "", raw).strip()
    text = MARKDOWN_LINK.sub(r"\1", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.replace("`", "")
    text = re.sub(r"\\([\\`*{}_\[\]()#+.!-])", r"\1", text)
    return html.unescape(text)


def github_slug(raw: str) -> str:
    text = heading_text(raw).lower().strip()
    # Approximate GitHub's heading slugger: retain Unicode letters/numbers,
    # underscores and existing hyphens; remove punctuation; map whitespace.
    text = "".join(char for char in text if char.isalnum() or char in "_-" or char.isspace())
    return re.sub(r"\s", "-", text)


def document_anchors(path: Path, errors: list[str], cache: dict[Path, set[str]]) -> set[str]:
    if path in cache:
        return cache[path]
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as exc:
        fail(f"{path.relative_to(ROOT)}: cannot inspect link fragments: {exc}", errors)
        cache[path] = set()
        return cache[path]

    prose = prose_without_fences(text.splitlines(), path.relative_to(ROOT), errors)
    anchors = {unquote(value) for value in EXPLICIT_ANCHOR.findall(prose)}
    counts: dict[str, int] = {}
    for line in prose.splitlines():
        match = HEADING.match(line)
        if match is None:
            continue
        base = github_slug(match.group(1))
        if not base:
            continue
        duplicate = counts.get(base, 0)
        anchors.add(base if duplicate == 0 else f"{base}-{duplicate}")
        counts[base] = duplicate + 1
    cache[path] = anchors
    return anchors


def validate_document(
    path: Path, errors: list[str], anchor_cache: dict[Path, set[str]]
) -> str:
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
            target.path.relative_to(ROOT)
        except ValueError:
            fail(f"{relative}: local link escapes repository: {match.group(1)}", errors)
            continue
        if not target.path.exists():
            fail(f"{relative}: broken local link: {match.group(1)}", errors)
            continue
        if target.fragment is None:
            continue
        if not target.path.is_file():
            fail(f"{relative}: fragment target is not a file: {match.group(1)}", errors)
            continue
        anchors = document_anchors(target.path, errors, anchor_cache)
        if target.fragment not in anchors:
            fail(f"{relative}: missing local fragment: {match.group(1)}", errors)

    return text


def require(text: str, path: Path, values: tuple[str, ...], errors: list[str]) -> None:
    relative = path.relative_to(ROOT)
    for value in values:
        if value not in text:
            fail(f"{relative}: required reader checkpoint missing: {value}", errors)


def main() -> int:
    errors: list[str] = []
    anchor_cache: dict[Path, set[str]] = {}
    texts = {
        path: validate_document(path, errors, anchor_cache)
        for path in DOCS
    }

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
            "RL_IMAGE_DIGEST",
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
            "sha256sum --check \"$selected_checksum\"",
            "selected asset must have exactly one checksum entry",
            "command -v runnerloom",
            "backup --out",
            "既存ファイルを上書きしません",
            "mktemp -d",
        ),
        errors,
    )

    troubleshooting_path = ROOT / "docs" / "TROUBLESHOOTING.ja.md"
    troubleshooting = texts[troubleshooting_path]
    require(
        troubleshooting,
        troubleshooting_path,
        (
            "umask 077",
            "credential JSON structure: OK",
            "調査情報を保存する",
        ),
        errors,
    )

    combined = "\n".join(texts.values())
    forbidden = {
        'findmnt -T "$RL_NODE_STATE/images"': "image cache may not exist before first Agent start",
        "--out /var/backups/runnerloom/controller.db": "backup destinations must be new files",
        "sha256sum --check --ignore-missing SHA256SUMS": "selected artifact could be absent",
        "disable --now runnerloom-agent-node-a.service": "configured Node names are not always node-a",
        "sudo jq . /var/lib/runnerloom/controller/github-credentials.json": "diagnostics must not print identifiers and paths",
    }
    for value, reason in forbidden.items():
        if value in combined:
            fail(f"reader docs contain unsafe copy/paste command {value!r}: {reason}", errors)

    if errors:
        print("Documentation validation failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    print(f"Validated {len(DOCS)} reader-facing documents, fences, links and fragments.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
