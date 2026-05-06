---
name: nt-plan
version: 1.2.0
description: |
  Deep codebase planning for sdk-proxy. Given a beads issue, systematically
  explores internal packages, finds existing patterns to reuse, and produces a
  structured implementation plan. Designed to run under claude-opus-4-7 for
  maximum architecture reasoning quality. Called by /next-task but can be
  invoked standalone for any issue: /nt-plan <issue-id>
allowed-tools:
  - Bash
  - Read
  - Grep
  - Glob
  - WebSearch
  - WebFetch
  - AskUserQuestion
---

## Inputs

You will be given:
- `ISSUE_ID`: the beads issue identifier (e.g. `beads-042`)
- The full output of `bd show <ISSUE_ID>`

If invoked standalone (user typed `/nt-plan`), ask: "Which issue ID should I plan?"

---

## Exploration Sequence

**Batch 1** — In a **single message**, run all three in parallel:

```bash
cat CLAUDE.md
```

```bash
cat Makefile
```

```bash
cat cmd/proxy/main.go
```

**Batch 2** — Scoped doc.go reads. Based on the packages the issue directly names or implies, issue a **single parallel message** reading only the relevant `doc.go` files. Do NOT read doc.go files for unrelated packages — the doc.go summaries are sufficient context for packages not being modified.

For ARCHITECTURAL tasks (new abstractions, cross-cutting, 3+ packages): read all package doc.go files in one parallel message.
For STANDARD tasks: read only the doc.go files for packages directly mentioned in the issue.

**Batch 3** — For every package you plan to modify, read the full implementation file **and** its test file in a single parallel message. Do not read full files for unrelated packages.

---

## Grep for Reuse Before Designing

In a **single message**, run all grep commands in parallel before proposing any new type, interface, or function:

```bash
grep -rn "^type.*interface" internal/
```

```bash
grep -rn 'fmt.Errorf.*%w' internal/
```

```bash
grep -rn 'metrics\.' internal/
```

```bash
grep -rn 'slog\.' internal/
```

```bash
grep -rn 'ctx context.Context' internal/
```

If a suitable implementation already exists, reuse or extend it. Never create a new abstraction when one exists.

---

## Design Principles

Apply these principles during planning:

- **SRP**: Each type and function has one reason to change. If a struct is doing two things, split it.
- **DIP**: New dependencies are injected as interfaces, not instantiated inline. Wire everything in `cmd/proxy/main.go`.
- **ISP**: Interfaces are narrow — define the minimal interface the consumer needs, not everything the implementation provides.
- **KISS**: Prefer the simplest solution that satisfies the requirements. Don't add layers of abstraction for hypothetical future use. Three similar lines beat a premature abstraction.
- **No `init()`**: Wire all dependencies explicitly in `main`. No side effects at package load time.

---

## Clarification Gate

If the issue description is ambiguous on any of these points, use `AskUserQuestion` to resolve them before producing the plan:

- Which existing interface to implement or extend?
- Should the new component be sync or async?
- What should happen on error (retry? log and continue? return 503?)
- What metric names should be used?

Do not make large assumptions. A wrong assumption here is more expensive than the cost of asking.

---

## Package → Docs Mapping

When determining which docs files need updating, use this reference:

```
internal/cache     → docs/cache.md, docs/configuration.md
internal/sse       → docs/sse.md, docs/monitoring.md
internal/snapshot  → docs/snapshot.md
internal/telemetry → docs/telemetry.md
internal/metrics   → docs/telemetry.md
internal/server    → docs/monitoring.md, docs/shutdown.md
internal/config    → docs/configuration.md
```

---

## Output Format

Produce the implementation plan as a structured markdown document in this exact order:

```
## Implementation Plan: <ISSUE_TITLE>

### ⚠️ Early Risk Signals
| Signal | Severity | Recommended action |
|--------|----------|--------------------|
| (none identified) | — | — |

Populate with:
- Invariant risks: any change touching token passthrough, cache key format, payload opacity, SSE fan-out, 503-on-miss
- Scope surprises: "issue mentions one package but implementation requires N"
- Non-blocking ambiguities worth flagging (not critical enough to block via AskUserQuestion)

If no risks: leave the table with the (none identified) row.

### Files Changed
| Path | Status | Reason |
|------|--------|--------|
| internal/foo/foo.go | NEW | Implements FooDoer interface |

### Reused Patterns
| Existing symbol | File:line | How reused |
|-----------------|-----------|------------|
| cache.SHA256Key | internal/cache/cache.go:42 | Same keying strategy for new store |

### New Interfaces
| Name | Location | Rationale |
|------|----------|-----------|
| FooDoer | internal/foo/foo.go | Narrow interface for the one caller |

### Test Strategy
| Test file | Tag | Behavior covered |
|-----------|-----|-----------------|
| internal/foo/foo_test.go | (none) | Happy path + error injection via mock FooDoer |

### OTel Metrics
| Metric name | Instrument | Where emitted |
|-------------|------------|---------------|
| sdk_proxy.foo.requests | Counter | internal/metrics/metrics.go |

### Docs to Update
| File | Section | What to verify or update |
|------|---------|--------------------------|
| docs/cache.md | TTL Revalidation | Update if TTL value changes |

Populate using the Package → Docs Mapping above. If no docs are affected, leave a single row: (none).

### Key Design Invariants Checked
- [ ] Token passthrough: no credentials stored; upstream requests use original SDK token
- [ ] Cache key: SHA-256(sdk_key)[:16] — raw SDK key never in logs/files/metrics
- [ ] Payload opaque: flagd config JSON treated as bytes, never parsed
- [ ] Snapshot writes async: failed snapshot never fails an SDK request
- [ ] SSE fan-out: N clients → exactly 1 upstream connection per SDK key
- [ ] 503 on uncached miss: never return empty config

### Conventional Commit
<type>(<scope>): <description>
```

Return this plan as your final output. The orchestrator will present it to the user for approval.
