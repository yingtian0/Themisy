# Your first small contribution

You can start with documentation, an offline example or a CLI usability fix.
You do not need cloud credentials, production access, a running service or the
maintainer's Agent tooling. Read [CONTRIBUTING](../../CONTRIBUTING.md), the
[Code of Conduct](../../CODE_OF_CONDUCT.md) and [security policy](../../SECURITY.md).

## 1. Choose one task

Search existing Issues/PRs, then choose a bounded
[starter task](../contributing/starter-tasks.md). Describe the intended change on
that Issue before working to avoid duplicate effort; public comments must not
contain suspected vulnerability details. Ask implementation questions there or
on [#45](https://github.com/yingtian0/Themisy/issues/45). No particular maintainer
is automatically assigned; named contacts are still tracked in #44.

The starter tasks avoid signatures, identity, Journal, Policy and execution
authorization. Ask for scope review before a task grows into one of those areas.

## 2. Prepare a clean checkout

Install Git and Go 1.26.9 or later. Fork the public repository if you do not have
push access, clone your fork, enter the checkout and create your working branch:

```bash
git switch -c codex/first-contribution
go version
go build -o /tmp/themisyctl-first-pr ./cmd/themisyctl
/tmp/themisyctl-first-pr offline-plan -at 2026-10-18T00:00:00Z examples/offline/release.json
```

Use a different disposable binary path if `/tmp/themisyctl-first-pr` already
belongs to you. Build may download public Go modules; the resulting offline CLI
does not need network access. This is a local file-based plan, not permission to
execute a deployment. Try the rejected example and expected exit code in the
[offline course](offline-quickstart.md). Do not run `start` or launch Compose for
a docs/CLI contribution.

## 3. Make and check the smallest change

Always inspect the diff and run `git diff --check`. Use the Issue's concrete
acceptance checks. Additional checks depend on what you changed:

| Change | Relevant local validation |
|---|---|
| Markdown | Check relative links and run any examples actually changed; do not claim untested service startup |
| Offline fixtures/CLI | `go test ./cmd/themisyctl ./internal/contract ./internal/profile ./internal/planner`; build/run the normal and rejected examples |
| Go formatting | `make fmt-check`; format changed files with `gofmt` |
| Shared or concurrent Go behavior | Related package tests plus necessary race/vet; explain the chosen boundary |
| Durable schema, protocol or safety boundary | Consult ADR/review requirements; not a starter task; real DB/Temporal tests need a separate disposable environment |

You do not need to run every test for a documentation correction. CI supplies
the full quality/race/scenario/integration/scan checks for the submitted commit.
`make test-integration` can migrate down and stop its PostgreSQL service; never
point it at an existing development/customer database or run it against shared
containers. If infrastructure is unavailable, say **not run**, not PASS.

## 4. Certify and open a PR

Stage only your intended files, then create a DCO signed-off commit:

```bash
git diff --check
git diff --cached
git commit -s
```

The sign-off certifies that you have the right to contribute the change under
Apache-2.0. Push your branch to your fork and open a draft PR to the repository's
main branch. Use the PR template: describe the problem/result, related Issue,
checks and their source commit, compatibility and remaining work. Include
sanitized evidence. Do not publish private security reports or credentials.

Watch the checks on that exact commit, address review feedback and preserve
clear scope. A successful CI run is not maintainer acceptance or production
readiness. Do not close a parent Issue whose acceptance criteria remain open.

## Record friction and improve this guide

Independent external completion has not yet been measured. After your first
attempt, share a non-sensitive observation on #45 (or in your PR):

| Field | Record |
|---|---|
| Source commit, OS and Go version | |
| Setup start / first successful relevant check / PR opened | |
| Task and prior familiarity | |
| Assistance or confusing instruction | |
| Actual checks, skipped checks, install/network blocker | |
| One proposed guide improvement | |

Do not include personal contact details, secrets or raw customer logs. Maintainers
should revise the guide after the first external observation and link the change
back to #45. This table is a measurement procedure, not a completed user trial.
