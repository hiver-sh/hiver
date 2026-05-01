---
name: implement-ticket
description: Implement a single ticket (T<n>) from DESIGN.md §20. Reads the ticket spec and its dependencies, pulls in the relevant design sections, and ships exactly the stated deliverable as one focused change. Use when the user says "implement T47", "do ticket 62", "T15 please", or similar.
---

# implement-ticket

Implement one coding ticket from DESIGN.md §20 — and *only* that ticket.

## Input

The user passes a ticket ID. Accept any of: `T47`, `47`, `ticket 47`, `t47`. If the input is ambiguous (multiple plausible IDs, or none), ask once before doing anything else.

## Workflow

### 1. Load context

- **Find the ticket.** Read DESIGN.md §20 and locate the entry. If the file has no §20 or the ID isn't found, stop and report it.
- **Read referenced design sections.** Each ticket implicitly belongs to a component — `T47` is in §20.8 (sandboxd), so read §3.3. `T11` is in §20.3 (spec) so read §5. Read sibling tickets in the same `20.x` subsection to understand the seam between this ticket and adjacent work.
- **Pull in any `(REQ-N)` tags.** Grep PRD.md for the requirement so you know what behavior must hold.

### 2. Verify dependencies

For every prereq in `(deps: Tx, Ty)`:

- Look in the repo for the code that ticket would have produced. Use the `20.x` subsection as the hint for *where* (e.g., `cmd/sandboxd/`, `internal/spec/`).
- If a dep is missing, **stop**. Report which deps are unimplemented; ask the user whether to do them first or stub them.

Do not paper over missing deps.

### 3. Plan

For non-trivial tickets, use `TodoWrite` to track:

- Files to create / modify
- Interface(s) to define (for "interface ticket" entries — those define the Go interface plus a minimal stub; impls live in their own tickets)
- Tests to write
- Verification commands you'll run

For trivial tickets (binary skeletons, type definitions, single-file additions) skip the plan and just do it.

### 4. Implement

Code must:

- **Match the deliverable verbatim.** No drive-by additions; no work that belongs to a neighboring ticket "while I'm here."
- **Follow repo conventions** (DESIGN.md §15): typed sentinel errors, `errors.Is`/`As` at boundaries, `context.Context` as first arg on every IO call, table-driven tests, `errgroup` for fan-out, `slog` with `tenant_id` / `sandbox_id` / `trace_id`.
- **Inline `(R<N>)` tags** the same way the rest of the codebase does — comments or commit-message-style annotations near the implementing code.
- **Include at least one test.** Table-driven unit test for pure logic; `testcontainers-go` integration test if the ticket touches PG/Kafka; a smoke test for binary skeletons (`-help` doesn't panic).
- **Add godoc** on exported types and exported functions.

### 5. Verify locally

Run, in order, and stop on the first failure:

```bash
go build ./...
go test -race ./<affected-paths>/...
golangci-lint run ./...   # if configured
```

For new binaries: `go run ./cmd/<bin> --help` must not panic.

If verification fails, fix the code — do not hand off broken work.

### 6. Mark the ticket complete

After verification passes, edit DESIGN.md §20 to flip the ticket's checkbox from `- [ ] **T<n>.**` to `- [x] **T<n>.**`. This is the single source of truth for which tickets are done; the skill, the user, and any other automation all read it.

**Do not** mark complete if:
- Verification failed (don't claim broken work as done).
- The ticket was deferred (e.g., a dep was missing and the user asked you to stop).
- Only part of the deliverable shipped (the ticket is "one focused change" — partial credit isn't a thing).

### 7. Hand off

Output exactly four things:

1. **Files changed** — paths with `+/-` line counts.
2. **Test result** — pass/fail summary, plus how many tests ran.
3. **Suggested commit message** — `T<n>: <title copied from DESIGN.md §20>`. Do **not** commit unless the user explicitly asks; per the global git rules, only commit when asked.
4. **Newly unblocked tickets** — scan §20 for tickets that listed this one as a dep. List their IDs so the user knows what's now ready to start.

## Rules

- **One ticket, one change.** Resist the urge to fix unrelated issues you notice along the way.
- **Interface tickets ship interfaces only.** Real impls are separate tickets — don't merge them.
- **If the design is stale.** If the ticket description in §20 conflicts with the rest of the design (e.g., names a backend that's been removed, references a deleted file), stop and report the discrepancy. Do not freelance an interpretation.
- **No commits, no pushes, no PRs unless asked.** Hand off the change ready-to-commit; let the user decide when to land it.

## Example

User: `implement-ticket T47`

You:

1. Read DESIGN.md §20.8 → T47 is "sandboxd binary skeleton: gRPC server for scheduler. *(deps: T1, T2)*".
2. Verify T1 (`go.mod`, repo layout) and T2 (proto definitions in `pkg/sandboxv1/`) exist.
3. Read §3.3 to understand sandboxd's role and the Runtime interface (T48, deferred).
4. Create `cmd/sandboxd/main.go` with a minimal gRPC server, health endpoint, slog setup, and a stub for the Runtime interface T48 will define.
5. Add a smoke test that the binary boots and `--help` works.
6. `go build ./cmd/sandboxd && go test ./cmd/sandboxd/...` — both pass.
7. Edit DESIGN.md §20.8: flip `- [ ] **T47.**` → `- [x] **T47.**`.
8. Hand off with: 3 files changed (incl. DESIGN.md checkbox flip), 1 test added, suggested commit `T47: sandboxd binary skeleton`, and a note that T48–T55 are now unblocked.
