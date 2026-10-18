# First-contribution guide verification

2026-10-18, macOS arm64, Go 1.26.9. The code baseline is `12e58f2`
([PR #54](https://github.com/yingtian0/Themisy/pull/54), including offline PR #53).
This documentation revision changes no executable behavior.

- Extracted clean tracked source with `git archive` into a new temporary directory.
- Built the CLI, ran CLI/Contract/Profile/planner tests, and ran normal/rejected
  offline fixtures: exit 0 and 1 respectively, both with the offline notice.
- Checked relative links in CONTRIBUTING, first-contribution guide and starter index.
- Checked the diff for whitespace errors. Public starter Issues #58–#62 include
  files, expected result, concrete checks, prerequisite #53 and consultation links.

Existing Go module/build caches were reused. This is clean-source developer
verification, not fresh-machine dependency installation, an independent external
participant or a measured first PR. The first external observation and subsequent
guide revision remain acceptance work under #45. Named contacts remain #44.
