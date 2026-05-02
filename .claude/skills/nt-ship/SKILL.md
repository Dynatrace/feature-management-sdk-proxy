---
name: nt-ship
version: 1.0.0
description: |
  Quality gates, commit, beads close, and push for sdk-proxy. Runs lint/test/vet
  in parallel, gates on user approval before committing, closes the beads issue,
  and offers to push. Can be run standalone after any implementation: /nt-ship
  Called by /next-task after code review passes. Requires ISSUE_ID to be set
  in context, or prompts for it if invoked standalone.
allowed-tools:
  - Bash
  - AskUserQuestion
---

## Preamble

```bash
git diff --stat HEAD
git branch --show-current
```

Print the changed files and current branch. If there are no changes since HEAD, print
"Nothing to ship — no staged or unstaged changes." and exit.

If invoked standalone, ask: "Which beads issue ID should I close after committing?"
Store as `ISSUE_ID`.

---

## Phase 1 — Quality Gates (parallel)

In a **single message**, issue all three commands simultaneously:

```bash
make lint 2>&1
```

```bash
make test 2>&1
```

```bash
make vet 2>&1
```

Wait for all three to complete. Display results.

If any fail:
1. Print the failing output
2. Diagnose the root cause (read the relevant source file if needed)
3. Fix the issue
4. Re-run **only the failing gate** — do not re-run all three
5. Repeat until all three are green

Do not proceed to commit until all three pass.

---

## Phase 2 — Commit

**Branch check**: if currently on `main`, create a feature branch first:

```bash
git checkout -b <type>/<scope>-<short-description>
```

Use the conventional commit type and scope from the implementation plan (feat/fix/refactor/test/chore/ci/docs/perf × cache/sse/telemetry/snapshot/server/metrics/config/proxy).

**Stage specific files** — never `git add -A` or `git add .`:

```bash
git add <file1> <file2> ...
git diff --cached --stat
```

Show the staged diff stat to the user.

**D-COMMIT — AskUserQuestion**: Confirm the commit.

Show:
- Commit message: `<type>(<scope>): <description>`
- Staged files (from diff --cached --stat above)
- All quality gates: green ✓

Options:
- A) Commit with this message (recommended)
- B) Edit the commit message — collect the revised message, then commit
- C) Cancel — unstage everything and abort

On approval:

```bash
git commit -m "$(cat <<'EOF'
<type>(<scope>): <description>
EOF
)"
```

---

## Phase 3 — Beads Update

```bash
bd close <ISSUE_ID>
```

Then attempt dolt sync (non-fatal if no remote):

```bash
bd dolt push 2>&1 || echo "[info] no bd remote configured — skipping dolt push"
```

---

## Phase 4 — Push Gate

**D-PUSH — AskUserQuestion**: Push to remote?

Present:
- Current branch
- Commit just created (`git log --oneline -1`)
- Note: CLAUDE.md mandates push at session close

Options:
- A) Push now (recommended — satisfies CLAUDE.md session-close protocol)
- B) I'll push manually later

On approval:

```bash
git pull --rebase
git push -u origin $(git branch --show-current)
git status
```

Verify `git status` shows the branch is up to date with origin. If it does not, print the status and ask the user to investigate.

---

## Completion

Print:

```
Ship complete.

Issue:   <ISSUE_ID> [CLOSED]
Commit:  <git log --oneline -1>
Branch:  <branch>
Gates:   lint ✓  test ✓  vet ✓
```
