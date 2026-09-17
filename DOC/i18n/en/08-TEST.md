# Server-com Testing Document

> Goal: know **what tests exist**, **how to run them**, **current measured results**, and **what still needs humans**.
> Related: [02-ARCHITECTURE.md](02-ARCHITECTURE.md), [05-INSTALL.md](05-INSTALL.md), [06-OPS.md](06-OPS.md), [07-BACKUP.md](07-BACKUP.md)

---

## 1. Test Layering Overview

| Layer | What it does | Who runs it |
|---|---|---|
| Compile / static checks | Guarantee it compiles, no suspicious constructs | Dev / CI, automatic |
| Unit + integration tests | Verify business rules function-by-function / interface-by-interface | Dev / CI, automatic |
| Architecture-discipline gate | Guarantee layering, red lines, doc consistency | CI, automatic |
| Coverage gate | Guarantee key packages aren't left bare | CI, automatic |
| End-to-end probe | Real HTTP full-link smoke | After deploy / before release, needs real machine |
| Performance baseline | Verify list / sync / concurrent throughput targets | Real machine, repeatable script |
| Manual acceptance | UI clicks, over-long paths, real-platform, etc. | Ops / acceptance personnel |

> In the project, `cmd/testsgate`, `cmd/depsguard`, `cmd/coveragegate` are all **standalone gate programs**.
> The local commands and CI commands are the same (CI runs the same commands).

---

## 2. Automated Gates (one command per layer)

Run in the repo root (`Server-com/`):

| Layer | Command | What it blocks |
|---|---|---|
| Compile / static | `go build ./... && go vet ./...` | Compile errors, suspicious constructs |
| Unit + integration | `NETDISK_TEST_DSN='...' go test ./... -count=1` | All Go cases (incl. real PG/Redis) |
| Integration not skipped | `go run ./cmd/testsgate` | "DSN missing so a whole group skips while CI is all green" kind of **false green**; names any skipped case |
| Data race | `go run ./cmd/testsgate -race` | Data races (needs gcc) |
| Architecture & doc discipline | `go run ./cmd/depsguard` | Kernel packages depending on HTTP layer / desktop discipline / red lines / doc vs ops-artifact consistency |
| Coverage | `go run ./cmd/coveragegate -profile <cover.out> -min 55` | Key package coverage dropping below threshold |
| End-to-end probe | `go run ./cmd/probesmoke -pass <password>` | Real HTTP full link (upload/download/share/WebDAV/audit/patrol…) |

> Integration tests need the `netdisk_test` DB + Redis. Without real PG/Redis the integration cases are skipped;
> `testsgate`'s job is to **watch for "any case silently skipped"**, avoiding false green in CI.

---

## 3. Unit + Integration Tests (status quo)

- Full run: **1042 cases pass**, covering 39 packages / **0 skipped** (cross-checked by name in `testsgate`).
- Coverage domains: auth tokens / upload finalization / object-lock concurrency / directory semantics / sync changes / shares / quota reconciliation / patrol / WebDAV.

**Layered criteria for performance-sensitive assertions** (e.g. concurrent finalize lock window P95):
- Primary criterion uses the **median** (a regression slows every single lock hold);
- The tail uses P95 < 500ms (absorbing machine jitter from "whole-package parallel + shared test DB").
- Counterexample verification: artificially sleep inside the lock critical section → the median immediately slows and the case goes red, showing it is "critical section widened" rather than "jitter".

---

## 4. End-to-End Probe (probesmoke)

One command runs a full-link smoke against the **running real service** (51+ steps): login → directory → upload (TUS/WebDAV/multipart) →
download/Range → share → sync event → audit → patrol.

```sh
go run ./cmd/probesmoke -base http://127.0.0.1:8080 -user admin -pass '<password>'
```

> When going through the proxy the SSE header is consumed by Nginx (`X-Accel-Buffering`), so **the probe should hit the app directly** (51/51 all pass);
> through the proxy it is 50/51, and the difference is explained (not a fault).

---

## 5. Performance Baseline (measured 2026-09-12)

Script: `deploy/verify/06-perf-baseline.sh` (self-contained, repeatable; numbers come from the script's actual output). Machine: Debian 13,
6 vCPU / ~2GB RAM / sequential write 2723.6 MB/s (local baseline).

| Acceptance point | Threshold | Measured | Verdict |
|---|---|---|---|
| 100k-row directory listing | P95 ≤ 500ms | **P95 107.2 ms** (default page limit=200) | Pass (≈4.7× margin) |
| Remote change awareness (`/changes` visible) | P95 ≤ 3s | **P95 78.1 ms** (multipart write) | Pass (≈37× margin) |
| Remote change awareness (SSE frame arrival) | P95 ≤ 3s | **P95 80.5 ms** | Pass |
| Concurrent upload 8×8MiB | — | **9.66 MB/s**, 8/8 success, 0 fail | Pass |
| Concurrent upload 16×8MiB | — | **10.57 MB/s**, 16/16 success, 0 fail | Pass (12 rate-limited then retried) |
| Concurrent download 8/16×8MiB | — | **1944 / 2580 MB/s** | Pass (page-cache hit, not disk throughput) |

> **Stated honestly**: at 16 concurrent uploads, `POST /tus` got **429 rate-limited** 12 times (`rate_limits.upload=10/s`),
> the script retried with backoff and all succeeded — this is **rate limiting working as designed**, not an upload failure. If acceptance requires "16 concurrent all pass at once",
> raise `rate_limits.upload` or lower concurrency. The 2GB/s download magnitude is "memory + virtual disk" speed, only showing the download path has no queuing or errors under concurrency.

### How to Reproduce

```sh
BASE=http://127.0.0.1:8080 USER=admin PASS='<password>' \
ROWS=100000 NREP=60 NFEED=30 SAR="8 16" UPFILE=8388608 \
sudo sh deploy/verify/06-perf-baseline.sh | tail -n +1
```

The end of the script prints (per-sample / histogram / quantiles / verdict line).
Probe data is cleaned up automatically by default; `KEEP=1` keeps it (for troubleshooting).

---

## 6. Acceptance Cross-Reference and Rerun Entry

| Acceptance domain | Rerun entry |
|---|---|
| Unit baseline / coverage | `go test ./... -coverprofile=cover.out` → `coveragegate` |
| Integration test item-by-item | `go test -tags=integration ./...` |
| Performance baseline | `deploy/verify/06-perf-baseline.sh` (real machine) |
| Deploy & upgrade | `deploy/verify/0[12567]-*.sh` (real-machine scripts, line by line) |
| Backup / recovery / PITR | `deploy/backup/*.sh` (drill scripts + ledger) |
| Monitoring & alerting | `curl /metrics`, `promtool check rules` |
| Object consistency / temp-file reclamation | `go test ./internal/storage/ -run 'ReapTemp|MismatchedObjectKeys' -count=1 -v` |
| Data race | `go run ./cmd/testsgate -race` |

---

## 7. Items Still Needing Manual Execution (don't fake coverage in automation)

| Item | Why automation can't cover it | Where logged |
|---|---|---|
| Windows over-long path on site | Needs a real drive letter and a `longPathAware`-effective environment | Client acceptance checklist |
| Antivirus locking-file retry behavior | Needs a real AV hook | Same as above (honestly marked "unverified") |
| UI clicks (login/tray/settings/conflict/remote browse/check update) | No UI automation | Client acceptance checklist |
| 24h client long run | Duration and real interactive sessions | Manual before release |
| Third-party drill per manual (backup/recovery) | Needs "another person" operating by the manual | Drill ledger |

---

## 8. Three Testing Disciplines

1. **Cannot-judge ≠ pass**: when a checker encounters "cannot judge", it must judge failure, not be treated as pass.
2. **Reverse verification is part of the assertion**: every key assertion must do one "break the code → the assertion must go red",
   otherwise you don't know what you're watching (the repo has repeatedly shown "still green after breaking it").
3. **Evidence must be re-runnable**: evidence = command + real output + the version at the time; pasting an output with no command source does not count as evidence.
