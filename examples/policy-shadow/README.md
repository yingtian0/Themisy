# Synthetic signed policy comparison

These are offline analysis fixtures, not deployable policy configuration.
`current.json` and `candidate.json` are signed with one generated development key;
only `public.key` is retained. `cases.json` contains three synthetic canonical
inputs. There are no real users, credentials or tenant records.

Run the command in [the shadow guide](../../docs/policy/shadow-comparison.md)
with the fixed `-at 2026-10-19T00:00:00Z`. Expect three cases and one changed case:
`frozen-service` is ALLOW in the current Bundle and DENY with `SERVICE_FREEZE` in
the candidate. `stable-service` remains ALLOW; `missing-subject` remains DENY.
The output explicitly says `OFFLINE_SHADOW_ONLY`. These decisions authorize no
Write and do not establish trustworthy CI/health/identity context.

The Bundles expire one hour after the fixture time. They should fail current-time
verification after that point. Do not change expiry, skip hash/signature checks
or install the sample public key to make an execution pass. The source-only CLI
example does not publish or apply either Bundle.
