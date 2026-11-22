# Plan: review fixes

## Approach

Five streams, one worker each (Sonnet), each in its own git worktree and branch cut from
`feat/restapi-feature-complete`. Streams own disjoint paths (see `tasks.md` Owns lines and the shared-file
rules). The orchestrator tracks `status.md`, resolves cross-stream requests and performs the merges.

## Phases

| Phase | What | Exit |
|---|---|---|
| P0 Setup | Create five worktrees and branches `fix/review-a` to `fix/review-e`; pre-created notes sections | Worktrees exist; `make gate` green in each at the start commit |
| P1 Stream A | The P0 fix, audit identity, denial audit | A merged; gate and cover green |
| P2 Stream C | Metrics recorded, alerts provable, condition text sanitised, new counters registered | C merged after rebase on A |
| P3 Streams B and D | B: gateway cap, settle queue, usage writer, wiring after C. D: readyz, replicas, timeouts, IAM, settings, then registrar list after A | Both merged; gate green; `make load` passes |
| P4 Stream E | HMAC fingerprint (needs D-5), import marshal errors, contract PR and exemption test | E merged; root PR open |
| P5 Close | Step 10 archive, Step 11 final quality pass, docs, README | Gate, cover, vet with tags, `make load` green |

## Merge order and why

1. **A** holds the P0 and edits `h2_common.go` (`w2Caller`), which D's FR-R15 later touches.
2. **C** registers metrics that B's wiring needs and rewrites alerts B's counters feed.
3. **B and D** are independent of each other. B's wiring commit (B-10) waits for C. D's FR-R15 (D-9, D-10) waits for A.
4. **E** needs D's `IdempotencyHMACKey` (D-5) in `settings` and `Config`.

After each merge: rebase the remaining worktrees onto the integration branch, run `make gate`, then
`make cover`.

## Risks

| Risk | Mitigation |
|---|---|
| Two streams edit `cmd/afs-api/wire.go` | Only D owns it; A and E have their own wire files; RD-5 avoids a Deps wiring edit |
| B writes metrics before C registers them | Names fixed in RD-3; B uses interfaces until B-10; B-10 waits for C |
| Restore validation adds a query per field | A-7 asserts no N+1 and the 200-agent bound |
| Alerts reference metrics that are never written again | C-5 dead-metric test runs in `go test ./...` |
| FR-R14 key name disagreement | Fixed as RD-2; D-5 early; E-1 waits |
| Deferred cluster tasks marked done by mistake | They are not tasks here; non-goal in `spec.md` |

## Definition of done

Every box in `tasks.md` ticked; `status.md` shows 100% with each stream row complete; `implementation-notes.md`
holds a design review per stream; the gate and coverage hold; the root contract PR is open.
