# Pinned Contract and Profile source collection

`contextsnapshot.NewMetadataCollector` is an opt-in read-only composition for
Service Contract and Release Profile provenance. Configure a tenant/service/
environment key with a GitHub owner, repository, exact lowercase 40-character
commit SHA, two repository-relative paths and a maximum observation age of one
minute to 24 hours. The configuration is copied at construction. No request may
supply a repository, revision, path, URL, document, credential or success flag.

The collector uses the existing GitHub credential broker in verification mode.
Supply a customer-managed, short-lived token source with read-only repository
contents permission. The broker does not inspect token scopes; deployment and
scope restrictions remain the operator's responsibility. The default client
uses HTTPS, a bounded timeout and refuses redirects. Injected HTTP clients and
token sources are trusted host dependencies, not caller input.

Only fixed `api.github.com` contents GET requests with the configured commit ref
are made. Branches, tags, path traversal and malformed selectors are rejected.
Responses must be bounded regular file records with the requested path, base64
content, matching byte count and matching Git blob SHA. Symlink-type and submodule
responses are refused; GitHub may resolve an in-repository symlink as a file, so
this is not a repository-tree mode attestation. No download URL is followed.
Git blob SHA-1 is recorded as a provider identifier; SHA-256 binds raw bytes and
the existing canonical Contract/Profile hashes bind validated semantic content.
GitHub's authenticated HTTPS API is the source authority; this is not an
independent signature or commit-tree/provenance attestation.

Existing strict decoders reject unknown fields, unsupported schema and multiple
documents. Contract service and selected environment must match the logical
key. The selected Profile must have the Contract's environment Profile name;
all its required capabilities must be present in that environment's Contract.
The declared application repository in Contract metadata is distinct from the
operator-configured repository containing the Contract file.

The snapshot privately owns validated bytes. `Contract()` and `Profile()` return
new decoded values with source references, so nested map/slice edits cannot
change the snapshot. These methods expose inspectable metadata, not a freshness
guarantee: call `RequireFreshMetadata(now)` at every authorization point. Records
and JSON cannot restore trusted snapshots. Recollect through the configured
source instead. Maximum age is time since source fetch, not commit age or proof
that the pinned revision is the latest branch head. Revision changes require an
operator configuration update and recollection.

Every successful collection commits `context.metadata.observe` before returning.
Audit records contain tenant/service/environment, source repository/revision/
path, kind/schema/name, blob ID, raw/canonical hashes and fetch time/freshness.
They omit raw documents, tokens and raw provider/parser errors. Partial or invalid
metadata, provider failure, audit failure/response loss, cancellation, clock
reversal or expiry during durable append returns no snapshot. No retry or cache
fallback converts a failure to success. An ambiguous append can leave an audit
record even when the caller receives no snapshot; PASS records validation at
capture, not authorization or evidence of successful delivery to the caller.

Validation: `go test -race ./internal/contextsnapshot`, and the dedicated
`make test-ci-observation-audit` fixture now also verifies metadata records across
PostgreSQL reopen and a server-refused audit INSERT. HTTP and credentials are
synthetic fixtures; live GitHub staging acceptance remains unverified.

This unit leaves command defaults, Planner, Workflow and signed Grant protocols
unchanged. Composition of CI, dependencies and metadata into a complete Context,
Plan/Grant hash binding, approval invalidation, key/revision update handling and
live source staging remain Issue #13 work. Collection grants no execution authority.

Design rationale: [proposed ADR-0018](../adr/0018-pinned-metadata-source-observations.md).
