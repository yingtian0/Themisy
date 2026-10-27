# Reviewing an approval

The approval card reads the selected tenant's durable Run and the Plan/step bound
to that approval. It shows the requested artifact, environment/planned deploy,
requester, delegated Agent, recorded policy reason, declared impact, verification
requirement, observation window, rollback mode and available evidence metadata.
The Run page separately shows requester/Agent and the Plan freshness deadline.

Current artifact, actual blast radius, health thresholds, approved rollback
destination/deadline and named responder are not all recorded in the existing
projection. The UI labels these unknown instead of deriving them from a newer
catalog or inventing them. Recorded observation time is shown when present;
freshness remains unknown without its source's freshness threshold. No runtime
verification row means MISSING, never PASS. Request CI/health flags do not prove
authenticated evidence. Identity/Context production integration remains #12/#13.

Inspect the collapsed pinned-context section to confirm Plan, Contract, Profile
and Context references. Hashes supplement the human-readable change; they do not
replace it. Resolve missing information with the designated operator before
approving. The declared team is not an assigned incident responder. If outcome,
rollback authority or evidence is unresolved, use supported pause/freeze and
read-only recovery controls; do not infer that automatic rollback mode guarantees
permission or a known destination.

Expired or unbound approvals cannot be approved in the UI. A POST revalidates
current tenant identity, CSRF, the displayed Plan hash and current bound
Run/step/deadlines, then delegates role/separation-of-duties checks to the existing
application service. A stale form receives HTTP 409 and requires refresh/new
authorization. No identity or roles are trusted from form fields. Missing older
Plan/step/freshness data is not silently filled from today's catalog: replan and
review through supported application flows. API/CLI authorization is unchanged;
these UI guards do not implement the full #51 lifecycle.

Earlier validation used normal, missing-evidence, expired, mismatched Plan/artifact,
stale-form, wrong-role and cross-tenant fixtures; legitimate submit, CSRF,
identity revalidation and HTML escaping are checked through the HTTP handler.
Related tests/race/vet passed locally. A synthetic HTML preview was generated,
but browser file-URL preview was blocked by browser policy, so browser appearance
and interactive visual QA were not completed. No alternative browser route was
used to bypass that restriction. Real staging/production evidence was not tested.

## Verification record (2026-10-27)

Baseline: main `488045e217177fe630f2a7324d03f40a2e5aaa24`; the additional
recorded-observation fixture is in the commit adding this guide.
`go test -race -shuffle=on -count=1 -v ./internal/ui` passed on macOS arm64,
Go 1.26.10. The tests render real templates through the UI HTTP handler and
submit forms with in-process, synthetic identity/Memory Store/Workflow fixtures:

- Normal card: pinned service, artifact, requester, delegated Agent, reason,
  declared impact, success conditions, missing rollback target and resolution
  team. Valid CSRF form returns 303 and uses reverified roles/subject, ignoring
  caller-provided actor/admin fields.
- Missing observation: MISSING, source/time unavailable and freshness unknown.
- Recorded synthetic PASS: source/time displayed; freshness, current artifact
  and rollback destination remain explicitly unknown.
- Expired approval/Plan, missing step, changed Plan/artifact, stale form and
  removed roles: invalid approval disabled or POST rejected, zero approval
  signals sent to Workflow.
- Cross-tenant Run/approval details rejected, HTML escaped and invalid CSRF
  refused. No live identity provider, provider Write or deployed ingress tested.

A temporary localhost server using those same synthetic fixtures was also
viewed in the in-app browser: recorded PASS, missing observation and expired
Plan/approval. Source/time and unknowns were visible; expired approval was
reported and the Approve control was disabled. Its disabled styling now uses
a muted background/opacity and a not-allowed cursor so it is visually distinct
from an available approval. This developer browser check does not measure
external usability or participant outcomes (#47). The earlier blocked file-URL
preview remains a historical result, not the basis for this localhost check.
The temporary fixture/server is not shipped. This completes the bounded #46 UI
scope without claiming production identity or Evidence integration.
