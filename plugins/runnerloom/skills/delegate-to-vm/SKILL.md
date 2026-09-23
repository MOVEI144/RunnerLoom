---
name: delegate-to-vm
description: Use when a coding task is long-running or parallelizable and the user wants it done in the background, in a sandbox, or by another agent (codex, claude, opencode, pi, gemini). Delegates it to a disposable RunnerLoom VM through the runnerloom MCP tools and later collects the pushed branch, draft PR or patch.
---

# Delegating work to RunnerLoom VMs

RunnerLoom works like a self-hosted cloud agent. Each task gets a fresh VM on the user's own hardware. The VM clones the repository, runs one coding-agent CLI with the credentials already on this PC, commits and pushes a work branch (optionally opening a draft PR), then the VM is deleted.

## When to delegate

- The work takes long, for example large refactors, migrations, flaky-test hunts, or dependency upgrades with full test runs.
- Several independent variants or subtasks can run in parallel. Start one task per subtask.
- The user explicitly wants another agent's opinion or implementation.

Keep short, interactive edits local.

## How

1. `runnerloom_list_agents` tells you which agents are `allowed`. Only the user can allow more, with `runnerloom client allow`. Never ask the VM agent to exfiltrate or print credentials.
2. `runnerloom_start_task` with a self-contained `prompt`, and with `repository` (`owner/name`) plus `baseRef` when the work is on a repository. The VM cannot see local uncommitted changes, so push first or describe them.
3. To iterate on a result, use `continueFrom: <taskId>`. The new task continues the same branch.
4. `runnerloom_get_task` shows `state`, recent `progress` and, when finished, the `result`:
   - `branch`, `commit`, `pullRequestURL` and `diffStat`
   - the agent's final `output`
   - a `patch` when nothing could be pushed (request it with `includePatch: true`)
5. A task in `Failed`, `Cancelled`, `Expired` or `Finished` without a result did not complete. Read `result.error` and `output` before retrying.
