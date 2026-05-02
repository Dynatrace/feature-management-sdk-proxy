---
name: nt-plan
version: 1.0.0
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

Read the following files in this order before designing anything:

```bash
cat CLAUDE.md
cat Makefile
cat cmd/proxy/main.go
```

Then read all package doc files to understand the current state of each package:

```bash
cat internal/cache/doc.go
cat internal/config/doc.go
cat internal/metrics/doc.go
cat internal/proxy/doc.go
cat internal/server/doc.go
cat internal/snapshot/doc.go
cat internal/sse/doc.go
cat internal/telemetry/doc.go
```

Based on the issue scope, read the full implementation files for the relevant packages. For every package you plan to modify, also read its test file to understand existing test patterns before designing new tests.

---

## Grep for Reuse Before Designing

Before proposing any new type, interface, or function, search the codebase for existing implementations:

```bash
# Find existing interfaces
grep -rn "^type.*interface" internal/

# Find error wrapping patterns
grep -rn 'fmt.Errorf.*%w' internal/

# Find how metrics are emitted
grep -rn 'metrics\.' internal/

# Find logger usage patterns
grep -rn 'slog\.' internal/

# Find context propagation patterns
grep -rn 'ctx context.Context' internal/
```

If a suitable implementation already exists, reuse or extend it. Never create a new abstraction when one exists.

---

## Design Principles

Apply these SOLID principles during planning:

- **SRP**: Each type and function has one reason to change. If a struct is doing two things, split it.
- **DIP**: New dependencies are injected as interfaces, not instantiated inline. Wire everything in `cmd/proxy/main.go`.
- **ISP**: Interfaces are narrow — define the minimal interface the consumer needs, not everything the implementation provides.
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

## Output Format

Produce the implementation plan as a structured markdown document:

```
## Implementation Plan: <ISSUE_TITLE>

### Files Changed
| Path | Status | Reason |
|------|--------|--------|
| internal/foo/foo.go | NEW | Implements FooDoer interface |

### Reused Patterns
| Existing symbol | File:line | How reused |
|-----------------|-----------|------------|
| cache.SHA256Key | internal/cache/cache.go:42 | Same keying strategy for new store |

### New Interfaces
| Name | Location | SRP/DIP rationale |
|------|----------|-------------------|
| FooDoer | internal/foo/foo.go | Narrow interface for the one caller |

### Test Strategy
| Test file | Tag | Behavior covered |
|-----------|-----|-----------------|
| internal/foo/foo_test.go | (none) | Happy path + error injection via mock FooDoer |

### OTel Metrics
| Metric name | Instrument | Where emitted |
|-------------|------------|---------------|
| sdk_proxy.foo.requests | Counter | internal/metrics/metrics.go |

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
