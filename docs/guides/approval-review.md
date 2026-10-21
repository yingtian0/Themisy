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

Validation uses normal, missing-evidence, expired, mismatched Plan/artifact,
stale-form, wrong-role and cross-tenant fixtures; legitimate submit, CSRF,
identity revalidation and HTML escaping are checked through the HTTP handler.
Related tests/race/vet passed locally. A synthetic HTML preview was generated,
but browser file-URL preview was blocked by browser policy, so browser appearance
and interactive visual QA were not completed. No alternative browser route was
used to bypass that restriction. Real staging/production evidence was not tested.
