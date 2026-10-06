# Runner manifest and recovery protocol

## Signed Plan Manifest

Version: `themisy.plan-manifest/v1alpha1`.

The signed payload is the JSON encoding of `protocol.PlanManifest` in declared
field order, with an empty Signature object, sorted approval references and UTC
timestamps. Unknown JSON fields, trailing JSON, invalid digests, duplicate/empty
approval references, unsupported operations and versions are rejected. This is
the repository's typed canonical encoding, not a claim of general JSON/JCS
canonicalization. Ed25519 signatures use base64url without padding. The distinct
version provides domain separation from Action Grant signatures.

Required bindings: issuer, tenant, Runner group, run/step, positive revision,
Plan/Contract/Profile/Policy/Evidence hashes, logical target, typed action,
approval requirement/references, issued_at and expires_at. Rollback binds the
original external execution ID. The manifest contains no arbitrary AWS payload,
Role ARN, cloud credentials or user identity token.

Provision a JSON array using a customer-controlled channel separate from Grant
delivery. The Runner verifies before pinning and again when resolving execution
context. PostgreSQL serializes the run's current revision; lower revisions and
same-revision content changes are rejected. Missing steps at the new revision
fail closed rather than falling back to a prior plan. A dispatch transaction
checks the reservation's revision/hash against the current revision under a
shared lock, serialized against revision activation.

## Recovery reports

`POST /v1/runner/grants/{id}/reconcile` uses the existing Runner transport version
and authentication. JSON fields are `protocol_version`, `request_hash`, `result`.
The request hash is the canonical original Action Grant hash. No delivery token
is required or persisted; a registered peer in the original tenant/group can
report a durable observed result after reconnect or replacement.

The server checks Grant ID, tenant/group, request hash, run/step, result version,
status and completion time. A CAS transaction updates a nonterminal/UNKNOWN
dispatch and appends an audit event. Identical reports are idempotent. Conflicting
terminal outcomes return 409 and are audited. The local `reported_version`
advances only after acknowledgement, so a lost response is safe to retry.

No recovery response authorizes execution. Provider reads happen through typed
adapters only; UNKNOWN/absent state remains unresolved, never a retry instruction.
Result/Evidence cryptographic signatures and full runtime health remain #9.
