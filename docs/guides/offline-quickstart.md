# Try planning locally in 15 minutes

This course is for Platform/SRE developers evaluating Themisy with ECS and an
existing CI/CD system. It uses local files only. No cloud credentials, Control
Plane, PostgreSQL, Temporal, or Runner is needed. It creates no grants or
approvals, acquires no credentials, and performs no adapter writes.

The inputs are synthetic. A successful plan proves structural validation and
planning only; it does not verify identity, CI, runtime health, policy approval,
or production readiness. Do not submit the example output as execution authority.

## Prepare

Install Go 1.26.9 or later and use a checkout containing this guide. From the
repository root, build the CLI:

```bash
go build -o /tmp/themisyctl ./cmd/themisyctl
```

The first build may download Go modules and needs internet access. After the
build, the commands below need no network. No secret files or environment
variables are required. The `/tmp/themisyctl` path is a disposable output; choose
another path if it already belongs to you.

## Generate a plan

```bash
/tmp/themisyctl offline-plan -at 2026-10-18T00:00:00Z examples/offline/release.json
```

Expected exit code: **0**. Standard error prints `OFFLINE ONLY`; standard output
is JSON with `mode: "offline"`, a notice, and `plan`. The single staging step is
`identity-api` at `sha-demo`, using `ecs-rolling-standard`. `plan.hash` and
`plan.plan_hash` match. Contract/Profile hashes and `context_snapshot_ref` are
computed by the same planner used by the service. Policy/Evidence hashes are
empty because this course has no verified policy/evidence snapshot.

`-at` fixes timestamps for reproducible comparison. Omit it to use current UTC
time. The historical example plan expires 15 minutes after the supplied time;
offline planning does not make an expired plan executable. Input, time, and
planner context held fixed produce the same full plan and hashes.

## See a rejection

```bash
/tmp/themisyctl offline-plan examples/offline/rejected.json
```

Expected exit code: **1**, no plan on standard output, and this stable reason on
standard error:

```text
themisyctl: unknown service "missing-api"
```

The `changes[0].service` in `examples/offline/rejected.json` has no corresponding
Service Contract. Change it to `identity-api` to resolve this rejection. Contract
graph errors also return existing reason codes such as `UNKNOWN_DEPENDENCY` and
identify the dependency field. The local test suite covers that case.

To try your own files, specify directories before the intent filename:

```bash
/tmp/themisyctl offline-plan -contracts examples/contracts -profiles examples/profiles examples/offline/release.json
```

All commands exit immediately; there is no service to stop or state to reset.
Delete only the CLI binary you created if you no longer need it. No database,
container, or cloud cleanup is involved.

## Record an independent trial

The target is normal and rejected examples reproduced without assistance in
15 minutes. Build/download time counts. Developer tests do not demonstrate that
external users meet this target. Use a fresh checkout and record:

| Field | Trial result |
|---|---|
| Commit, OS, Go version | Not measured |
| Participant / prior familiarity | Not measured |
| Start / first normal plan / rejected example times | Not measured |
| Assistance and confusing steps | Not measured |
| Normal exit 0 and rejection exit 1 | Not measured |
| Second use / own Contract / setup burden | Not measured |

Do not include credentials or personal data in shared observations. External
observations and continued-use evaluation are tracked in Issue #47.
