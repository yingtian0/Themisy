# External evaluation protocol

Status: prepared, no external participants observed. Source selection and results
are recorded in [trial results](results.md). Parent: #47, roadmap #52.

## Scope and participants

Invite Platform/SRE or operations developers who use ECS and existing CI/CD and
have not authored the offline course. Maintainer/developer self-checks are
separate engineering evidence and never count as external completions. Obtain
permission before recording or sharing observations. Recruitment and messages
to participants are human actions; this document sends no invitations.

Use a reviewed, fixed source commit or verified package and its offline guide.
Record OS/architecture, Go version if building, prior familiarity, dependency
cache state and package/source choice. Do not mix commits in one cohort without
reporting the difference. Start only after the participant can access the exact
guide; record download/build time as part of first-use time.

The initial task uses only local synthetic Contracts/Profiles. Request no cloud
credentials, customer data, production access or deployment. Own non-production
Contract editing can follow. Actual staging is a separate opt-in study only
after the relevant #11/readiness gates pass; it is not required for offline
completion and is not authorized by this protocol.

## Before the timed task: questions

Record paraphrased, anonymous answers; collect no internal identifiers:

1. Who currently requests, approves, executes and checks a service change? What
   tools and failure/recovery steps are involved?
2. What concrete recent problem would justify adding Themisy to CI/CD and IAM?
   What is already sufficient today?
3. What information do you need to approve an artifact/target change, including
   success criteria and the known rollback destination?
4. How much time do you spend collecting approval/health/audit evidence? What
   units/time period support that estimate?
5. What setup and ongoing maintenance burden would be acceptable? Who would own
   keys, Runner operation, allowlists, policy and incident recovery?
6. Would you consider moving ordinary CI's direct Write permissions to a
   dedicated Runner in non-production? Under which conditions would you refuse?

These are discovery questions, not a request to change anyone's IAM permissions.

## Timed offline task

Give only the fixed guide and this neutral task: generate a normal Plan, trigger
the documented rejection, and explain which input needs correction. Start the
clock before setup/download/build. Stop first-use time when both outcomes and
exit codes have been reproduced. Record the first normal-plan time separately.

Do not guide the participant through commands during the unassisted attempt.
Record every hint, external help source and command correction. If help is
needed, record the unassisted attempt's outcome/time, then offer help and record
the assisted completion separately. A participant can stop at any time; do not
erase failed starts or setup failures from the attempted cohort.

Record command output only when sanitized and shareable. No credentials,
personal contact details, raw customer logs or source contracts enter public
Issues. Participant and organization keys are anonymous study IDs; keep any
contact/consent linkage outside the public repository with restricted access.

## Follow-up tasks and repeat use

- After the timed task, ask the participant to add a second synthetic or safely
  anonymized Contract. Measure time separately, record the changed fields and
  whether assistance was needed. Do not count it as first-task success.
- Ask them to explain whether a Plan proves identity, health or permission to
  deploy. Misunderstanding these boundaries is a product/documentation finding.
- If they choose to return later, record a second actual use, its date and task.
  An intention or positive comment is not repeat use. Record an own non-production
  Contract/application separately from using provided samples.
- Compare the approval information, evidence-collection effort and setup/ongoing
  burden with the participant's existing method using the same stated task and
  time unit. Distinguish measured time from recollection; do not imply competitor
  superiority from a planning demo.

Do not automatically contact participants or schedule follow-ups from this file.
Record observations when they actually occur and have permission to be shared.

## Metrics and denominators

Initial target: 4 of 5 external attempted participants reproduce both outcomes
without assistance within 15 minutes. Count attempted users, including install
failures; separately report invited, declined and not-started counts. Never
remove an attempt because it failed. No attempts means not measured, not 0%
or 100% success. Assistance and overtime remain visible even if later completed.

Initial reach/contribution targets are three external reproduction reports and
three reproducible bug reports or external PRs. Count distinct actionable
contributions, retain links, and avoid counting one report in multiple categories
as several people. Continued-use targets are three organizations with two actual
uses and one own non-production Contract/environment application. Report people,
organizations, observation period and target separately from actual values.

## Improvement decision

For each cohort, identify the blocking step and link the smallest corrective
Issue/PR with source commit. Retest that change with a later independent attempt;
do not rewrite earlier results as successes. Revise the guide after the first
external contribution observation (#45).

If return use is absent, record which hypothesis failed before adding adapters:
target user mismatch, insufficient initial value, or excessive setup/maintenance
burden. Interview evidence may leave the answer uncertain; record that rather
than force a favorable verdict. Stars/downloads are reach signals, not demand,
safety or continued-use evidence.
