---
name: next-task
version: 1.2.0
description: |
  Full-cycle implementation workflow for sdk-proxy. Discovers the next ready
  issue via bd, classifies complexity, runs deep Opus planning (/nt-plan) for
  architectural work or lightweight inline planning for docs/trivial changes,
  gates on approval, implements on a feature branch, runs parallel Sonnet code
  review (/nt-review) for non-trivial changes, passes quality gates (including
  integration tests), commits, merges to main, and ships (/nt-ship).
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

## Phase 2 — Complexity Classification

**Before reading any sub-skill or spawning any agent**, classify the issue complexity using the details already in context from Phase 1.

```
LIGHTWEIGHT — skip nt-plan and nt-review entirely:
  - bd issue type == "docs"
  - OR title/description contains any of:
    "update docs", "fix docs", "clarify", "typo", "update readme",
    "documentation", "correct", "stale doc", "inaccurate"

STANDARD — use nt-plan with model=sonnet:
  - type is task or bug
  - scope touches 1–2 known packages
  - No new interfaces or abstraction layers mentioned

ARCHITECTURAL — use nt-plan with model=opus:
  - New interfaces or abstract types proposed
  - 3+ packages touched or mentioned
  - Cross-cutting concerns (cache + SSE + telemetry together)
  - Anything that affects key design invariants
  - Default for any ambiguous case
```

Store result as `COMPLEXITY_CLASS`.

---

## Phase 3 — Planning

### If COMPLEXITY_CLASS == LIGHTWEIGHT

Read only the specific files mentioned in the issue. Do NOT read `nt-plan/SKILL.md` or spawn a planning agent.

Present a brief change summary:

```
Proposed changes:
- <file>: <what will change and where>
- <file>: <what will change and where>
```

Use `AskUserQuestion` (D3-LIGHT):
- A) Proceed with these changes (recommended)
- B) Abort

If B: exit cleanly. Skip to end.

### If COMPLEXITY_CLASS == STANDARD or ARCHITECTURAL

Read the full issue and the sub-skill:

```bash
bd show <ISSUE_ID>
```

```
Read .claude/skills/nt-plan/SKILL.md
```

Spawn a **single Agent call** using those instructions as the system prompt:

```
model: sonnet   ← for STANDARD
model: opus     ← for ARCHITECTURAL
description: Deep implementation planning for <ISSUE_TITLE>
prompt:
  [Full contents of .claude/skills/nt-plan/SKILL.md]

  ---

  ISSUE_ID: <ISSUE_ID>
  ISSUE_TITLE: <ISSUE_TITLE>
  COMPLEXITY_CLASS: <COMPLEXITY_CLASS>

  Issue details:
  <full output of bd show <ISSUE_ID>>
```

The agent will explore the codebase, grep for reuse opportunities, check invariants, and return a structured implementation plan. The plan will include a "Conventional Commit" line with `<type>(<scope>): <description>` — store these values as `COMMIT_TYPE`, `COMMIT_SCOPE`, and `COMMIT_DESC`.

**Render the plan in full**, leading with the "⚠️ Early Risk Signals" section.

Use `AskUserQuestion` (D3):

**Question**: Ready to proceed with this implementation plan?

Options:
- A) Approve — claim the issue and start implementing (recommended)
- B) Revise — collect feedback, re-run Phase 3 with feedback appended, loop back to D3
- C) Abort — exit without claiming or modifying anything

**If B**: Collect revision notes. Re-run the planning Agent with original details plus:
```
REVISION FEEDBACK:
<user's notes>
```
Loop until approved or aborted.

**If C**: Exit cleanly.

Do NOT claim the issue yet — wait for plan approval.

---

## Phase 4 — Implementation

Immediately after approval, claim the issue:

```bash
bd update <ISSUE_ID> --claim
```

**For LIGHTWEIGHT tasks**: implement the changes described in the brief summary. No feature branch required for docs-only changes — commit directly on `main` unless the scope grows.

**For STANDARD/ARCHITECTURAL tasks**: create the feature branch first:

```bash
git checkout -b <COMMIT_TYPE>/<COMMIT_SCOPE>-<short-description>
```

Derive the branch name from the plan's conventional commit: type and scope as-is, description lowercased and hyphenated (max 30 chars). Example: `feat/cache-ttl-refresh`.

Execute the approved plan step by step. Follow these rules:

- Implement in this order: interfaces → implementation → tests
- **Update every file listed in the plan's "Docs to Update" table** as part of this phase — docs updates go in the same commit as the code changes
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

**Skip entirely for LIGHTWEIGHT tasks.**

For STANDARD/ARCHITECTURAL tasks:

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
1. Run `make lint`, `make test`, `make vet`, `make test-int` in parallel
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

  Issue:      <ISSUE_ID> — <ISSUE_TITLE> [CLOSED]
  Complexity: <COMPLEXITY_CLASS>
  Commit:     <git log --oneline -1>
  Branch:     merged into main, deleted  (or: committed on main for LIGHTWEIGHT)
  Gates:      lint ✓  test ✓  vet ✓  test-int ✓
  Review:     security ✓  DX ✓  (or: skipped — LIGHTWEIGHT)

Run /next-task again to pick up the next issue.
```
