---
name: nt-review
version: 1.1.0
description: |
  Two-perspective parallel code review for sdk-proxy. Spawns Security and
  Developer Experience reviewer agents simultaneously against the current git
  diff. Each reviewer runs on claude-sonnet. Returns collated findings grouped
  by severity. Can be invoked standalone after any code change: /nt-review
  Called by /next-task after implementation is complete.
allowed-tools:
  - Bash
  - Agent
  - AskUserQuestion
---

## Preamble

```bash
git diff HEAD | wc -l
git diff HEAD
```

Capture and store the full diff output. Note the line count.

If the diff is empty, print "Nothing to review — no changes since last commit." and exit.

If the diff exceeds 400 lines, use only the first 400 lines and append:
```
[DIFF TRUNCATED at 400 lines — full diff available via `git diff HEAD`]
```

---

## Parallel Review

In a **single message**, issue both Agent calls simultaneously. Do not wait for one agent before issuing the next. Append the diff (full or truncated as above) to each prompt, after the word "DIFF:".

---

### Agent A — Security

```
model: sonnet
description: Security review
prompt:
You are a security engineer reviewing a Go diff for an SDK proxy service.

Security invariants that MUST NOT be violated (from CLAUDE.md):
- Token passthrough: proxy holds no credentials; every upstream request uses the
  SDK token from the originating request — NEVER store or log raw SDK tokens
- Cache key: SHA-256(sdk_key) truncated to 16 hex chars — raw SDK keys must NEVER
  appear in files, object storage, metric labels, or log messages
- Payload opaque: the proxy never parses flagd config JSON — treat as bytes only
- 503 on uncached miss: never return empty config (indistinguishable from empty flag set)

Review the following diff for:
1. SDK key/token handling: any raw SDK key in logs, files, metrics, or error messages?
2. Input validation at system boundaries: any unsanitized user input used unsafely?
3. Error message leakage: do errors expose internal structure or credentials to callers?
4. HTTP client safety: are timeouts set? Is TLS verification enabled?
5. Goroutine safety: mutex coverage correct? No concurrent map writes without sync?
6. DoS vectors: unbounded buffers? Missing timeouts in request paths? Expensive ops on hot path?

Return valid JSON only — no prose before or after:
{
  "summary": "one-sentence overall verdict",
  "findings": [
    {
      "severity": "error|warning|info",
      "file": "path/to/file.go",
      "line": 42,
      "issue": "concise description of the problem",
      "recommendation": "how to fix it"
    }
  ],
  "approved": true
}

DIFF:
[append the diff here]
```

---

### Agent B — Developer Experience & Observability

```
model: sonnet
description: Developer experience and observability review
prompt:
You are a developer advocate reviewing a Go diff for developer experience,
operational clarity, and long-term maintainability.

Observability requirements (from CLAUDE.md):
- Emit OTel metrics (not Prometheus-native); all metric names defined in internal/metrics
- Every dropped record (billing overflow, cache miss during outage, malformed request)
  MUST produce a structured log entry with enough context to diagnose the cause
- New config fields must have sane defaults and be validated at startup

Review the following diff for:
1. Self-documenting names: can you understand what each type/function does without a comment?
2. Export hygiene: are exported fields intentional? Anything accidentally public?
3. Actionable error strings: do errors name what failed and why? ("failed to fetch config for key %s: %w")
4. OTel instrumentation: are new code paths covered by metrics and structured log entries?
5. Startup validation: are new config fields validated in internal/config?
6. Testability: can new types be tested without live upstream services?

Return valid JSON only — no prose before or after:
{
  "summary": "one-sentence overall verdict",
  "findings": [
    {
      "severity": "error|warning|info",
      "file": "path/to/file.go",
      "line": 42,
      "issue": "concise description of the problem",
      "recommendation": "how to fix it"
    }
  ],
  "approved": true
}

DIFF:
[append the diff here]
```

---

## Collation

After both agents return, collate findings across both reports and group by severity:

**Errors** (block implementation — must fix before proceeding):
- List all `"severity": "error"` findings with file, line, and recommendation

**Warnings** (require user decision):
- List all `"severity": "warning"` findings
- For each warning, use `AskUserQuestion`: fix now, or defer as a new bd issue?

**Info** (noted, no action required):
- List all `"severity": "info"` findings as reference

Print a summary table:
```
| Reviewer | Approved | Errors | Warnings | Info |
|----------|----------|--------|----------|------|
| Security | ✓/✗      | N      | N        | N    |
| DX       | ✓/✗      | N      | N        | N    |
```

Return to the caller (or print, if invoked standalone) once all errors are resolved and warnings are either fixed or deferred.
