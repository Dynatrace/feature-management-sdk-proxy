---
name: next-task
version: 1.1.0
description: |
  Full-cycle implementation workflow for sdk-proxy. Discovers the next ready
  issue via bd, runs deep Opus planning (/nt-plan), gates on approval,
  implements on a feature branch, runs parallel Sonnet code review (/nt-review),
  passes quality gates, commits, merges to main, and ships (/nt-ship).
  Each sub-skill can be invoked independently.
  Use when asked to "pick up the next task", "next task", "work on the backlog",
  "implement the next issue", or simply "let's get to work".
allowed-tools:
  - Bash
  - Read
  - Edit
  - Write
  - Grep
  - Glob
  - Agent
  - AskUserQuestion
  - mcp__language-server__definition
  - mcp__language-server__hover
  - mcp__language-server__references
  - mcp__language-server__diagnostics
---

## Preamble (run immediately on skill start)

In a **single message**, run all three in parallel:

```bash
bd ready 2>&1
```

```bash
git branch --show-current
git status --short
```

```bash
go build ./... 2>&1
```

Surface the issue queue, repository state, and build status before any user interaction.

---

## Phase 0 — Dirty Tree & Build Check

**Build check**: If `go build ./...` produced any output (errors/warnings), surface it and use `AskUserQuestion`:
- A) Abort — fix the build before starting (recommended)
- B) Continue — the build failure is expected / will be addressed in this task

**Dirty tree check**: If `git status --short` shows uncommitted changes, use `AskUserQuestion`:
- A) Stash changes and continue (`git stash`) — recommended if changes are unrelated
- B) Continue with the dirty tree — only if the uncommitted work is intentional
- C) Abort — let me clean up first

If stash chosen, run `git stash` before proceeding.

---

## Phase 1 — Task Discovery

For each of the top 2–3 candidates shown by `bd ready`, run `bd show <id>` in parallel (single Bash message) to get full details.

Use `AskUserQuestion` (D1):

**Question**: Which issue should we implement this session?

Show the top candidates with their titles, priorities, and a one-line description.

Options:
- A) `<id1>`: `<title>` — highest priority, no blockers (recommended)
- B) `<id2>`: `<title>` — second candidate
- C) Enter a specific issue ID — if none of the above fit
- D) Nothing to implement — abort

Store the selected issue as `ISSUE_ID` and `ISSUE_TITLE`.

---

## Phase 2 — Deep Planning (Opus)

Read the full issue:

```bash
bd show <ISSUE_ID>
```

Read the sub-skill instructions:

```
Read .claude/skills/nt-plan/SKILL.md
```

Then spawn a **single Agent call** using those instructions as the system prompt:

```
model: opus
description: Deep implementation planning for <ISSUE_TITLE>
prompt:
  [Full contents of .claude/skills/nt-plan/SKILL.md]

  ---

  ISSUE_ID: <ISSUE_ID>
  ISSUE_TITLE: <ISSUE_TITLE>

  Issue details:
  <full output of bd show <ISSUE_ID>>
```

The agent will explore the codebase, grep for reuse opportunities, check invariants, and return a structured implementation plan. The plan will include a "Conventional Commit" line with `<type>(<scope>): <description>` — store these values as `COMMIT_TYPE`, `COMMIT_SCOPE`, and `COMMIT_DESC`.

Do NOT claim the issue yet — wait for plan approval.

---

## Phase 3 — Plan Review Gate

Render the plan returned by the planning agent in full.

Use `AskUserQuestion` (D3):

**Question**: Ready to proceed with this implementation plan?

Options:
- A) Approve — claim the issue and start implementing (recommended)
- B) Revise — I have feedback; collect it inline, re-run Phase 2 with the feedback appended to the issue context, then re-present at D3
- C) Abort — exit without claiming or modifying anything

**If B**: Collect the user's revision notes. Re-run the planning Agent with the original issue details plus:
```
REVISION FEEDBACK:
<user's notes>
```
Loop back to D3 until approved or aborted.

**If C**: Exit cleanly. No claim made, no files touched.

---

## Phase 4 — Implementation

Immediately after approval, claim the issue and create the feature branch:

```bash
bd update <ISSUE_ID> --claim
git checkout -b <COMMIT_TYPE>/<COMMIT_SCOPE>-<short-description>
```

Derive the branch name from the plan's conventional commit: type and scope as-is, description lowercased and hyphenated (max 30 chars). Example: `feat/cache-ttl-refresh`.

Execute the approved plan step by step. Follow these rules:

- Implement in this order: interfaces → implementation → tests
- After writing each new file, run a quick build sanity check:
  ```bash
  go build ./...
  ```
- If `mcp__language-server__*` tools are available, use them to navigate to existing definitions rather than grepping — prefer `mcp__language-server__definition` for jumping to a symbol's definition
- OTel metrics: always add new metric definitions to `internal/metrics/metrics.go` — never define metrics inline
- SDK keys: never log or label with raw SDK keys — use `SHA-256(sdk_key)[:16]` (see existing pattern in `internal/cache/cache.go`)
- No `init()` functions — wire all new dependencies in `cmd/proxy/main.go`
- Snapshot writes must be async — a failed snapshot must never fail an SDK request; log + metric only
- Do NOT commit during implementation — that happens in Phase 6

---

## Phase 5 — Code Review

Read the review sub-skill:

```
Read .claude/skills/nt-review/SKILL.md
```

Follow its instructions from top to bottom. It will:
1. Capture `git diff HEAD`
2. Spawn two parallel review agents (Security, DX) on `claude-sonnet-4-6`
3. Collate findings by severity
4. Gate warnings on user decision via AskUserQuestion

Fix all `"severity": "error"` findings before continuing. Defer or fix warnings per user decision.

---

## Phase 6 — Quality Gates, Commit & Close

Read the ship sub-skill:

```
Read .claude/skills/nt-ship/SKILL.md
```

Follow its instructions from top to bottom. It will:
1. Run `make lint`, `make test`, `make vet` in parallel
2. Fix any failures
3. Gate the commit on user approval (D-COMMIT)
4. Close `<ISSUE_ID>` in beads
5. Merge the feature branch into `main`
6. Gate the push on user approval (D-PUSH)

Pass `ISSUE_ID` and the current feature branch name through to the ship sub-skill.

---

## Phase 10 — Wrap Up

Print a clean summary:

```
Session complete.

  Issue:    <ISSUE_ID> — <ISSUE_TITLE> [CLOSED]
  Commit:   <git log --oneline -1>
  Branch:   merged into main, deleted
  Gates:    lint ✓  test ✓  vet ✓
  Review:   security ✓  DX ✓

Run /next-task again to pick up the next issue.
```
