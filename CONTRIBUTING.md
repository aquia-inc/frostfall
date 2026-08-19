# Contributing

Thanks for considering a contribution. This document covers the mechanics;
DESIGN.md covers the architecture and the contracts that are expensive to
change.

## Development setup

Requirements: Go (version in `go.mod`) and a Chrome or Chromium install
(the test suite launches a real browser; CI runners have one preinstalled).

```bash
go build ./...
go vet ./...
go test ./...          # includes real-browser tests; go test -short skips them
```

The end-to-end check scans a fixture page with planted violations:

```bash
go run ./cmd/frostfall --config testdata/static/frostfall.yml
# exits 1 by design - the fixture's enforcing config finds 4 violations
```

## The contracts

Some parts of Frostfall are frozen contracts (see DESIGN.md): the violation
fingerprinting scheme, exit codes, the config schema's existing keys, and the
action interface. Changes to these need a migration story, not just a diff -
fingerprint changes invalidate every user's committed baseline. The
fingerprint stability tests in `internal/fingerprint` are the executable form
of that contract; if your change breaks one, the burden is on the change.

## Commits and pull requests

- Conventional commits (`feat:`, `fix:`, `docs:`, `ci:`, `chore:`, `test:`,
  `refactor:`). Releases are cut automatically from commit types on merge:
  `feat` = minor, `fix`/`perf`/`docs`/`refactor` = patch.
- One concern per PR. Include tests for behavior changes.
- PRs merge by squash; the PR title becomes the release-facing commit subject,
  so write it like a changelog line.
- CI must pass: build, vet, tests, and the fixture integration scan.

## Filing issues

Bug reports with a minimal `.frostfall.yml` and a target page (or HTML
snippet) get fixed fastest. For accessibility-rule questions (why did axe
flag this?), the rule's Deque University link in the report is the reference;
issues here are for Frostfall's behavior, not axe's rules.
