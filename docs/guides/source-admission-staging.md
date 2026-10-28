# External source-admission staging acceptance

Status: **not executed**. The project owner confirmed on 2026-10-28 that the
staging environment is not prepared. This document is a procedure, not test
evidence. #13 remains open for this gate; fixture results must not be relabeled.

## Prerequisites

Provision a dedicated non-production tenant, allowed GitHub repository/workflow,
real successful CI run/attempt and actual deployable artifact digest. Produce
its build statement under the independent build identity. Provision reviewed
immutable Contract/Profile sources, a real dependency metric query and
read-only source credentials. Separate build/source/identity/Policy/manifest/Grant
trust and workloads as described in [source setup](source-admission.md).

Apply migrations to disposable customer and Control Plane databases. Record
source commit, CLI/tool versions, public key IDs, tenant and source selectors.
Keep credentials in protected workload files and never put their values, raw
HTTP headers/bodies or logs in reports. This procedure does not authorize live
cloud Writes or production rollout; read-only source checks can be completed
with deployment adapters unavailable.

## Positive and refusal course

1. Collect real CI, trusted metadata and dependency observations; independently
   verify the build statement. Record signed source revision/hash, CI run/attempt,
   artifact digest, metadata repo/revision/path/hash, observation windows and
   freshness bounds. Atomically provision the signed file to both consumers.
2. Create a Plan with the same target/artifact and deliberately untrusted input
   CI/health flags. Confirm the Plan derives verified facts and pins all source
   identities/hashes. Do not infer a deployment from successful planning.
3. Use a different commit, artifact or metadata selector and confirm refusal.
   Save sanitized reason codes and verify no new Grant/credential/adapter call.
4. In this isolated course, use revoked/read-invalid or rate-limited source access,
   a partial provider response, and FAIL/MISSING/INCONCLUSIVE dependency states.
   Publish each negative revision. Confirm no cached PASS or earlier signed
   revision restores authorization. Do not revoke a shared/customer production key.
5. Let a snapshot expire and renew with a new revision. Confirm old Plan/approval
   is rejected and the new context requires replan/reapproval. Repeat after
   closing/reopening both DB pools/processes.
6. If separately authorized, run the controlled non-production Write course
   with independent approved Manifest and verified current authority. Prove
   a source change between final validation and durable admission, or before
   Runner Write, prevents a new operation. Keep provider Write counts and
   deployment credential counts distinct from source-read credentials.

## Evidence and completion

Keep a versioned sanitized report with the exact application commit, real source
identifiers, signed input/output hashes and revisions, clocks/windows, expected
and actual four-value results, stable refusal reasons, audit references and call
counts. Distinguish actual providers from fixtures. Hash/pin the report artifacts
and have the maintainer review scope and omissions before marking the staging
checkbox or closing #13. No invented run, artifact, metric or external measurement
can substitute for this course.
