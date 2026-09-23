---
description: Hand a long-running coding task to an agent in a disposable RunnerLoom VM
argument-hint: "[agent] <what the VM agent should do>"
---

Delegate this task to a RunnerLoom VM: $ARGUMENTS

1. Call `runnerloom_list_agents` and `runnerloom_list_pools`. Use only an agent whose `allowed` is true. If the user named an agent that is not allowed, stop and tell them to run `runnerloom client allow <agent>` on this PC. Do not try to change the allow list.
2. If the work concerns the current repository, find its GitHub `owner/name` with `git remote get-url origin` and its current branch. Pass them as `repository` and `baseRef`.
3. Write a complete, self-contained `prompt`. The VM agent cannot see this conversation or local uncommitted files. Include the goal, constraints, the files involved, how to verify (test commands), and "commit your work" guidance.
4. Call `runnerloom_start_task`. Set `pullRequest: true` only when the user wants a PR.
5. Report the task ID and the work branch. Explain that the task runs asynchronously. Do not poll in a tight loop. Check it again only when the user asks, or with a single `runnerloom_get_task` call with `waitSeconds` up to 50.
