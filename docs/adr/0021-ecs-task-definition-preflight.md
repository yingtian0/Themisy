# ADR-0021: ECS registered task-definition preflight

- Status: Proposed
- Date: 2026-10-24
- Related: Issue #7, ADR-0008

A trusted mapping names a task-definition revision and expected image digests,
but the write path should check its registered container images before dispatch.
Read the exact definition using the purpose-scoped SDK client before deploy or
rollback. Require the matching ARN, ACTIVE state, exact unique container set and
immutable image digests. A failed read cannot be replaced by a write or a cached
positive. Keep runtime rollout/health verification after dispatch.

The internal ECS client interface gains one read operation. Existing request and
signed protocols are unchanged. Customer role policies need the additional read
permission; AWS currently does not support resource-level scoping for this
DescribeTaskDefinition action. No role, resource, task-definition registration or
write permission is created by the implementation. Live IAM/staging remains a
separate acceptance gate. This decision stays Proposed pending maintainer review.

Tests cover mismatched ARN, inactive definition, mutable/wrong images, missing,
extra and duplicate containers and provider failure for both deploy and rollback,
with zero UpdateService calls on all refusal paths.
