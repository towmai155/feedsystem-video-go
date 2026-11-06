# P0 load tests

Message durability, retries, deduplication, and the latest verification:
[P0-MESSAGE-RELIABILITY-20261023.md](P0-MESSAGE-RELIABILITY-20261023.md).

To run real broker-outage, Redis processing failure, quarantine/replay and
duplicate-delivery checks before the regular load stages:

```powershell
./test/load/run-p0.ps1 -ProjectName feedsystem-p0-reliable-next -FaultChecks -Seconds 5 -Levels '100,300' -Cases 'feed_auth_hot,feed_anon_cold'
```

This deliberately stops/restarts only the selected test project's RabbitMQ
container after checking its Compose ownership label. It writes only the fresh
isolated fixture. Use a fresh project name per run; do not combine with ReadOnly.
Write reconciliation waits for the outbox, actual notification/heat effects,
and queues. A zero runner exit code still requires checking verification flags.

After fixing the cause of a quarantined event, replay a bounded batch from the
backend directory, using the intended environment's CONFIG_PATH:

```powershell
go run ./cmd/replay-dlq -queue video.popularity.events -limit 10
```

The tool preserves event IDs and only removes the original DLQ delivery after
confirmed republication. It rejects legacy DLQ entries without matching source
metadata; those require manual triage. Published outbox records remain available
for audit; set a retention policy and monitor pending events/DLQs for deployment.

Current repair/retest results and remaining Windows cold-start failures:
[P0-RETEST-20261023.md](P0-RETEST-20261023.md).

For the Linux API/Worker comparison, stop the previous test project first (the
loopback ports are shared), then run `./test/load/run-p0-linux.ps1`. This uses
separate `feedsystem-p0-linux` containers and existing Redis Alpine images as
the runtime for statically linked Go binaries. The generator still runs on
Windows; server/dependency networking also differs from the native run, so this
comparison does not isolate the server OS alone. Results include resolved Compose
configuration and hashes. The Linux runner stops its containers without deleting
data; a rerun requires a fresh empty fixture, as with the native runner.

Run from the repository root in PowerShell with Docker Desktop running:

```powershell
./test/load/run-p0.ps1
```

Default: 20 seconds per read stage, 25/100/300 concurrent clients, 2,000
unique-account requests per write stage. A complete run takes several minutes.
Use `-Seconds`, `-Levels`, and `-WriteRequests` to change the bounded workload.
For example, a plumbing smoke run is:

```powershell
./test/load/run-p0.ps1 -Seconds 1 -Levels '2' -WriteRequests 10
```

To retest only the database-dependent read paths, using a separate fresh test
project and the unchanged 3-second HTTP deadline:

```powershell
./test/load/run-p0.ps1 -ProjectName feedsystem-p0-fix -ReadOnly -Cases 'feed_auth_hot,feed_anon_cold' -Seconds 30 -Levels '25,100,300'
```

Only one test project can run at a time, because their loopback ports are shared.
Before rerunning a nonempty `feedsystem-p0-fix` fixture, stop and remove only
that disposable project's containers and volumes with the corresponding Compose
project name. The original baseline containers/data need not be removed.
The API/Worker profiling listener exposes `GET /debug/dbstats` when profiling
is enabled. Results sample physical connection counts, pool wait counts/time,
and connection-close reasons under `api_pool` / `worker_pool`.

The script starts **dedicated** MySQL, Redis, and RabbitMQ containers under the
Compose project `feedsystem-p0`; binds their ports only to loopback; compiles
the real API, Worker, and a Go load generator; and creates 6,000 fixture actors
plus a separate author and 50,000 fixture videos. API/Worker run natively on
Windows. It neither uses nor flushes the normal project's database or Redis.
Tokens are seeded using the application's signing code; measured requests
still execute the real JWT/session validation and existing rate limiters.
The regular API/Worker source and business configuration are unchanged.

Scenarios: anonymous and authenticated hot Feed, anonymous cold Feed, hot and
distributed video details, hot detail cache eviction every second, unique-user
hot-video likes/comments with Worker completion checks, and a bounded duplicate
like race. Pagination checks traverse 120 pages across the hot/cold boundary.
For each write batch the runner checks accepted request identities, database
rows/counters, Redis minute-bucket popularity, notification rows, and queue drain.

Results are written to `test/load/results/<timestamp>/` (gitignored):

- `REPORT.md`, `results.json`: throughput, latency percentiles for all attempts,
  status distributions, semantic response failures, exact write reconciliation,
  periodic DB/Redis/MQ/goroutine observations.
- `native-resources.csv`: sampled cumulative CPU time, RSS, private memory,
  thread and handle counts for the three native processes.
- `api.log`, `worker.log`: application logs; `resources-*.jsonl`: post-stage
  Docker resource snapshots, not peaks.
- `revision.txt`, `working-tree.txt`, `host.json`, `docker-info.json`: provenance
  and machine/runtime context. Fixture tokens are excluded from results.

This uses **closed-loop concurrency**, not constant arrival rate. A stage's
elapsed time includes its in-flight completion tail. Read responses are checked
for expected IDs and page shape. Valid RPS excludes non-200 and malformed
responses. HTTP success on writes is checked separately from eventual effects.
Row-visibility latency is sampled by polling every 250ms during load and drain;
it includes polling/query delay. Counters, Redis and notifications are separately
reconciled at the end. Queue backlog is sampled;
management stats can lag. No production SLA or pass threshold is assumed.
The generator shares a machine with API/Worker and virtualized dependencies;
these results cannot establish production or multi-instance capacity.

API/Worker are stopped at completion and dependencies are stopped unless
`-KeepDependencies` is supplied. To preserve the report while resetting **only
this test's disposable containers and their data** before another run:

```powershell
docker compose -p feedsystem-p0 -f test/load/compose.p0.yml down --volumes
```

The runner deliberately refuses a nonempty fixture database/Redis. An exit code
of zero means the run completed: consult the report's verification flags and
HTTP failures to determine correctness. Never reuse these fixtures or ports
for real data.
