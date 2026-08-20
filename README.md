<p align="center">
  <img src="assets/frostfall_no_bg_logo.png" alt="Frostfall logo" width="300">
</p>

# Frostfall

[![Release](https://img.shields.io/github/v/release/aquia-inc/frostfall)](https://github.com/aquia-inc/frostfall/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Accessibility testing for rendered web apps. Frostfall drives a headless browser,
runs [axe-core](https://github.com/dequelabs/axe-core) against your actual pages -
including the states behind clicks, like open modals and expanded menus - and
reports WCAG and Section 508 violations. Works as a local CLI and as a GitHub Action.

Sibling to [Emberfall](https://github.com/aquia-inc/emberfall): Emberfall smoke-tests
your HTTP APIs, Frostfall audits your UI. Same idea - a single static Go binary and
a declarative YAML config - pointed at the rendered DOM instead of the wire.

By default Frostfall **reports** violations without failing your build. Enforcement
is opt-in, and a baseline file means adopting it on a codebase with existing
violations doesn't nuke your CI on day one.

## Why Frostfall exists

If you build software for the federal government, accessibility is not a
nice-to-have. Section 508 makes it a legal requirement, agencies audit against
it, and a failed review can block your release. But when a developer sits down
to actually deal with it, the tooling landscape is genuinely confusing: a dozen
overlapping tools, most of them Node packages with different engines and
different opinions, browser extensions that only check the page you're looking
at, and Lighthouse scores that say 100 while a reviewer files findings against
the exact same page. Knowing *what to run, when, and what the output means for
your compliance posture* is a real problem, and most teams solve it with a
spreadsheet and somebody's memory.

Frostfall is our attempt to narrow that down to one tool with one workflow:

- **One binary, one config, no ecosystem to assemble.** No Node toolchain, no
  plugin matrix, no "which of the six axe wrappers do we use." Install it,
  point it at your app, get findings.
- **It tests what reviewers actually test.** Real rendered pages, including
  the states behind clicks - open modals, expanded menus, forms after a
  validation error. Most automated findings that surprise teams in a 508
  review live in those states, not on page load.
- **The output maps to how compliance work actually happens.** Existing
  violations get baselined as known debt instead of failing every build on
  day one. New violations fail loud. Fixed ones get pruned. The HTML report
  is something you can hand to a reviewer or attach to a POA&M without
  editing.
- **It fits both moments where accessibility work happens**: locally while
  you build, and in CI where it becomes a gate. Same config, same results.

What it deliberately does not claim: automated scanning covers roughly a third
to half of WCAG. No tool can verify that alt text is meaningful, that a screen
reader announces your flow sensibly, or that a human can complete your forms
with assistive technology. Frostfall exists to catch everything automation
*can* catch, continuously and early, so your manual testing time and your
reviewer's attention go to the problems that actually need a human.

We built it because we needed it on our own federal work, and Emberfall showed
us the shape a tool like this should take.

## Install

Homebrew:

```bash
brew tap aquia-inc/frostfall
brew install frostfall
```

From source:

```bash
go install github.com/aquia-inc/frostfall/cmd/frostfall@latest
```

Or download a release binary from the [releases page](https://github.com/aquia-inc/frostfall/releases).

Docker (the image bundles a pinned Chromium, so nothing else is needed).
Pass your uid so reports, screenshots, and baselines are writable on the
bind mount (the image's own user cannot write host-owned directories):

```bash
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" \
  ghcr.io/aquia-inc/frostfall:v1 --serve dist --format html
# attach mode against a server on your host needs host networking:
docker run --rm --network host --user "$(id -u):$(id -g)" -v "$PWD:/work" \
  ghcr.io/aquia-inc/frostfall:v1 --base-url http://localhost:5173
```

Frostfall needs a Chrome or Chromium. It finds your system install automatically
(GitHub Actions runners have one preinstalled); if none exists it downloads a
managed Chromium on first run. Override with `--browser-path`.

## Five-minute setup

**1. Generate a starter config** in your frontend project:

```bash
cd your-frontend-project
frostfall init
```

`init` detects your project type (Vite, Next.js, CRA, SvelteKit, Astro, or plain
static) and your package runner (yarn, pnpm, npm), and writes `.frostfall.yml`
with a matching server block and commented examples.

**2. Tell it where your app runs.** Open `.frostfall.yml` and pick exactly one
server mode:

```yaml
server:
  serve: ./dist                      # A: Frostfall serves a static build itself
  # baseUrl: http://localhost:5173   # B: attach to a server you already run
  # command: yarn dev                # C: (planned) Frostfall starts the dev server
```

Mode A is best for CI - no dev server, and you scan the production bundle
instead of a dev build full of dev-only DOM. Mode B is best while developing:
keep your dev server running and point Frostfall at it. Command-line flags
`--serve` and `--base-url` override this block without editing the file.

**3. List your pages** under `tests:`. Start with a few routes:

```yaml
tests:
  - id: home          # stable name - used in output, filtering, and the baseline
    path: /
  - id: pricing
    path: /pricing
```

If your app uses a **hash router**, the route lives in the fragment - write
`path: "/#/users"`, not `/users` (a plain path would just render your root page).

**4. Run it:**

```bash
frostfall
```

That's a report - violations are printed, exit code is 0. Nothing fails until
you opt in (step 6).

**5. Add stateful tests** for the places page-load scans can't reach.
Accessibility bugs live in states: the open modal, the expanded menu, the form
after a validation error. Steps drive the page there:

```yaml
  - id: checkout-modal
    path: /cart
    scan: false                  # skip the initial page, scan only the modal
    steps:
      - click: "#open-checkout"
      - waitFor: "[role=dialog]" # wait for something that proves readiness
      - scan:
          label: modal-open
          scope: "[role=dialog]" # audit only the modal subtree
```

**6. Baseline, then enforce.** Accept the violations that already exist:

```bash
frostfall --update-baseline      # writes .frostfall-baseline.json - commit it
```

Then turn on enforcement in the config:

```yaml
defaults:
  expect:
    severity: serious            # exit 1 on NEW violations at this impact or worse
```

From now on, baselined violations are reported as known debt but never fail the
run - only new ones do. When you fix old ones, the report tells you to prune:

```
3 baselined violation(s) no longer occur — run --update-baseline to prune
```

## Config reference

Every key, with defaults. Unknown keys are an error (exit 2), not a warning -
a typo in an accessibility config means silently not scanning. `${VAR}` and
`${VAR:-default}` interpolate environment variables in any string value.

```yaml
version: 1                       # required, must be 1

name: my-app                     # optional: app identity for report headers.
                                 # When omitted, detected automatically from
                                 # GITHUB_REPOSITORY (CI), the git remote
                                 # origin (org/repo), or package.json name.

server:                          # where the app comes from - exactly one mode
  serve: ./dist                  # serve this directory on an ephemeral port
  spaFallback: true              # serve mode: unknown paths fall back to index.html
  baseUrl: http://localhost:5173 # or: attach to an already-running server
  command: yarn dev              # or: spawn the dev server (planned, not yet implemented)

defaults:                        # applied to every test unless overridden
  standard: wcag21aa             # wcag2a | wcag2aa | wcag21a | wcag21aa | wcag22aa
                                 #   | section508 | best-practice
                                 # each expands to axe's cumulative tag set;
                                 # section508 = the 508 tag + WCAG 2.0 AA
  viewport: { width: 1280, height: 800 }
  waitFor: networkIdle           # page readiness: networkIdle | load | CSS selector
  settleTime: 500ms              # extra settle after waitFor (late paints, hydration)
  timeout: 30s                   # per-test budget: load + steps + scans
  rules:                         # per-rule toggles for every scan
    color-contrast: on
  expect:                        # ABSENT = report-only, always exit 0
    severity: serious            # fail on new violations at this impact or worse:
                                 #   minor | moderate | serious | critical
    maxViolations: 0             # tolerated count of failing violations (default 0)
    rules: [image-alt]           # also fail on these rule ids at ANY impact

baseline: .frostfall-baseline.json   # known-debt file; see step 6 above

profiles:                        # named overlays for environment differences
  ci:                            # "ci" auto-applies in GitHub Actions
    server:                      # may override: server, defaults, baseline,
      serve: ./dist              # discover - NEVER tests (one test list for
    defaults:                    # every environment, so coverage can't fork)
      expect:
        severity: serious

discover:                        # tuning for --discover (see Discovery below)
  maxDepth: 3                    # crawl depth from /
  maxPages: 100                  # hard cap on pages visited
  exclude: ["^/logout"]          # Go regexes matched against paths

tests:
  - id: home                     # required, unique; keep it stable - it is part
                                 # of every violation's baseline identity
    path: /                      # relative to the server's base URL
    # url: https://example.com/  # or an absolute URL (exactly one of path/url)
    scan: true                   # scan the initial state after load (default true)
    waitFor: "[data-testid=app]" # per-test readiness override
    rules:                       # per-test rule toggles (merged over defaults)
      color-contrast: off
    expect:                      # per-test enforcement override
      severity: critical
    steps: []                    # see the step table below
```

### Steps

Each step is one action. Deliberately small - enough to reach a state worth
scanning, not a general E2E framework.

| step | example | what it does |
|------|---------|--------------|
| `click` | `click: "#open-modal"` | wait for the selector, click it |
| `fill` | `fill: { "#email": "${TEST_EMAIL}" }` | focus, clear, type (fires input events); multiple fields run in document order |
| `press` | `press: Tab` | send a keyboard key (`Tab`, `Enter`, `Escape`, arrows...) - useful for testing keyboard reachability |
| `hover` | `hover: ".menu-trigger"` | mouse over (tooltips, hover menus) |
| `select` | `select: { "#country": "Canada" }` | choose an option in a `<select>` |
| `waitFor` | `waitFor: "[role=dialog]"` | wait for a selector, `networkIdle`, or `load` |
| `wait` | `wait: 500ms` | fixed sleep - escape hatch of last resort |
| `goto` | `goto: /settings` | navigate to another path mid-test |
| `scan` | `scan: { label: modal, scope: "[role=dialog]" }` | run the audit now; `label` names the scan point, `scope` limits it to a subtree, `rules` overrides per-scan |

A step that fails (selector never appears within the test timeout) is an
environment error (exit 3), not a violation.

If a page's results are flaky, replace `waitFor: networkIdle` with a CSS
selector that only exists when the page is genuinely ready - that is almost
always the fix.

## Command-line reference

```
frostfall [flags]          run the tests in the config
frostfall init             detect the project and write a starter .frostfall.yml
```

| flag | default | meaning |
|------|---------|---------|
| `--config PATH` | `.frostfall.yml`, then `frostfall.yml` | config file to use |
| `--base-url URL` | - | attach mode: scan an already-running server (overrides the config's `server` block) |
| `--serve DIR` | - | static mode: serve DIR with the embedded file server (overrides `server`) |
| `--baseline PATH` | value of `baseline:` in config | baseline file location |
| `--update-baseline` | off | accept all current violations into the baseline and exit 0 |
| `--id REGEX` | - | run only tests whose id matches (Go regex) |
| `--path REGEX` | - | run only tests whose path matches (combines with `--id`) |
| `--discover` | off | crawl same-origin links from `/` and scan discovered pages in addition to configured tests |
| `--screenshots` | off locally, **on in GitHub Actions** | capture an element-cropped PNG of each new violation |
| `--screenshot-dir DIR` | `frostfall-artifacts` | where screenshots are written |
| `--profile NAME` | `ci` in CI when defined, else none | apply a named profile overlay from the config; `none` forces the base config even in CI |
| `--browser-path PATH` | auto-detect | use this Chrome/Chromium binary instead of detection |
| `--format NAME` | `text` | report format: `text` (terminal), `html` (single-file report), or `sarif` (GitHub code scanning) |
| `--output PATH` | `frostfall-report.html` / `frostfall.sarif` | where to write the formatted report |
| `--gh-issues` | off | file/maintain GitHub issues for failing violations (CI; needs `GITHUB_TOKEN`, `GITHUB_REPOSITORY`) |
| `--gh-issues-dry-run` | off | print the issue actions without calling GitHub |
| `--validate` | off | check the config and exit (0 = valid, 2 = invalid) |
| `--verbose` | off | log page loads, steps, and scans to stderr |
| `--version` | - | print the frostfall and embedded axe-core versions |

In development, not yet functional: json and junit formats, and `--watch`
(rescan on change).

The regex filters make the common CI split easy: a fast subset on pre-push or
PR checks, the full suite on merge - for example `--id 'smoke-.*'`.

### Exit codes

| code | meaning |
|------|---------|
| 0 | ran to completion, nothing broke the enforcement contract (report-only mode always exits 0) |
| 1 | new violations broke the configured `expect` contract |
| 2 | invalid config |
| 3 | environment failure (browser, server unreachable, page load, step failure) |

Distinct codes mean CI can tell "the pages are broken" from "the tool couldn't
run".

## Profiles: one config, different environments

Local runs and CI runs legitimately differ - attach to a dev server locally,
serve the production build in CI; report-only locally, enforce in CI. Profiles
express that in one file:

```yaml
server:
  baseUrl: http://localhost:5173   # local default: attach to your dev server

profiles:
  ci:
    server:
      serve: ./dist                # CI: scan the production build
    defaults:
      expect:
        severity: serious          # CI: fail on new serious+ violations
```

The `ci` profile applies automatically in GitHub Actions; use `--profile ci`
to test it locally or `--profile none` to suppress it in CI. Any other name
(`staging`, `nightly`) is available via `--profile NAME`.

A profile may override `server`, `defaults`, `baseline`, and `discover` -
**never `tests`**. Overrides work in both directions: a profile can add
enforcement for CI, and a `local` profile under an enforcing base can relax
back to report-only with `expect: {}`. That restriction is the point: a second config file lets the
test list drift until CI silently covers less than local. With profiles, one
test list serves every environment by construction. Precedence is base config,
then profile, then command-line flags.

For values rather than structure, environment interpolation is often enough:
`baseUrl: ${APP_URL:-http://localhost:5173}`.

## Discovery: scan pages you didn't list

```bash
frostfall --discover
```

Crawls same-origin links breadth-first from `/` and scans every page it finds,
in addition to your configured tests. Explicit tests always win on a path
collision - discovery adds breadth, your hand-written tests keep the depth.

It understands real SPAs:

- **Hash routers** (`/#/users`) are treated as distinct pages, not collapsed
  into `/`.
- **Numeric route parameters** are deduplicated by shape: a data table linking
  to `/items/1` through `/items/1000` gets one representative scan, not a
  thousand.

Discovered pages get generated test ids (`discovered:/#/users`) and use your
defaults. Two caveats: a crawler cannot reach the states behind clicks, so keep
modals and flows as explicit step tests. And discovered ids come from your
data - if a route slug changes when your database is reseeded, baseline entries
for it go stale. Treat discovery as a scout: run it periodically, then promote
interesting pages into explicit tests with stable ids. Gate CI on explicit
tests against seeded, deterministic data.

## Screenshots

```bash
frostfall --screenshots
```

Captures an element-cropped PNG of each new violation into `frostfall-artifacts/`,
referenced from the report. Seeing the actual low-contrast button beats reading
a CSS selector. On by default in GitHub Actions, off locally. Add the artifacts
directory to your `.gitignore`.

## HTML report

```bash
frostfall --screenshots --format html
```

Writes `frostfall-report.html`: a self-contained single file - styling inline,
screenshots embedded - made to be attached to a ticket or handed to a reviewer
with nothing else. It opens with the run date, target, standard, profile, and
tool versions, summary cards (new / baselined / fixed / tests run), then one
triage table: severity, rule (linked to the fix documentation), page, element,
and the evidence screenshot. Failing violations sort first, baselined ones sink
to the bottom dimmed. Print-friendly for the compliance binder.

## Baseline details

Violations are matched by a fingerprint designed to survive DOM churn - all of
these are normalized away:

- hashed class names (emotion/MUI `css-*`, styled-components, CSS modules)
- generated element ids and React `useId` attribute values
- sibling reordering (`:nth-child` is dropped when anything better identifies
  the element)

The baseline records the axe-core version it was created with. When the binary
embeds a different axe version, Frostfall warns instead of reporting phantom
"new" violations; run `--update-baseline` to realign.

## GitHub Action

```yaml
- uses: aquia-inc/frostfall@v1
  id: a11y
  with:
    serve: ./dist
    baseline: .frostfall-baseline.json
- uses: actions/upload-artifact@v4
  if: always()
  with:
    name: frostfall-report
    path: |
      frostfall-report.html
      frostfall-artifacts/
```

The action writes the HTML report by default and exposes job outputs
(`new-violations`, `baselined-violations`, `stale-baseline-entries`,
`tests-run`, `report-file`) so downstream steps can gate:

```yaml
- if: steps.a11y.outputs.new-violations != '0'
  run: echo "::warning::${{ steps.a11y.outputs.new-violations }} new a11y violations"
```

For code scanning alerts, run a second step with `format: sarif` and upload:

```yaml
- uses: aquia-inc/frostfall@v1
  with:
    serve: ./dist
    format: sarif
    output: frostfall.sarif
- uses: github/codeql-action/upload-sarif@v3
  if: always()
  with:
    sarif_file: frostfall.sarif
```

Violations land in the **Security tab** as code scanning alerts (scanning
rendered pages, there is no source line to annotate, so no inline PR
annotations - the alert carries the page URL and selector). Alert identity is
the violation fingerprint, so alerts stay stable across runs instead of
churning. Baselined violations are omitted from the upload: known debt opens
no alerts, and baselining an existing violation closes its alert on the next
upload. In report mode the check stays green; add an `expect` block (or a
`ci` profile) when you want red builds.

Pin `@v1`. The action version is the tool version - there is no separate
version input to drift. `@v1` resolves to the newest release under that major
only, and every downloaded binary is sha256-verified against the release
checksums.

The action supports Linux and macOS runners. Windows binaries ship with every
release for local use, but the action's install step does not support Windows
runners yet.

## GitHub issues from CI

```bash
frostfall --gh-issues            # in CI: needs GITHUB_TOKEN + GITHUB_REPOSITORY
frostfall --gh-issues-dry-run    # print what would be filed, no credentials needed
```

Files one issue per rule per page (not per element - the same defect on seven
table rows is one bug), labeled `frostfall`, listing the affected selectors
and the fix link. Runs are idempotent: a hidden marker in each issue body
deduplicates across CI runs, so re-running never files duplicates. The
lifecycle goes both ways - a fixed violation gets a comment and its issue
closed, a recurrence reopens the old issue instead of filing a new one, and
filtered runs (`--id`) never close issues for pages they didn't scan. Issues
without the marker are never touched, so hand-filed accessibility issues are
safe. Only violations that would fail an enforcing build are filed; baselined
debt is not. The workflow needs `issues: write` permission. Filing failures
are warnings - they never break the scan.

## How it compares

| | Frostfall | pa11y / pa11y-ci | Lighthouse CI | axe CLI |
|---|---|---|---|---|
| Engine | axe-core | HTML_CodeSniffer (axe optional) | axe-core (curated subset) | axe-core |
| Runtime | Go binary + system Chrome (managed download fallback) | Node + Puppeteer Chromium download | Node + Chrome | Node + WebDriver |
| Interaction states | steps with labeled, scoped scans per journey | per-URL action scripts, one scan per URL | none (page load only) | none |
| Regression model | fingerprint baseline: grandfather existing, fail on new, prune fixed | threshold counts | score budgets | none |
| SPA discovery | hash-router aware crawl, route-shape dedup | none | none | none |
| Reports | terminal, self-contained HTML with screenshots, SARIF | CLI, JSON, CSV, HTML | HTML, JSON | CLI, JSON |
| GitHub integration | code scanning alerts with stable identity, issue filing with lifecycle | via custom glue | status checks | via custom glue |

Where the others are ahead: pa11y has a decade of production use, a dashboard
project, and HTML_CodeSniffer's per-WCAG-criterion output that some auditors
prefer; Lighthouse CI tracks performance and SEO alongside accessibility and
has first-class score budgets. If your need is a performance-plus-a11y score
trend, run Lighthouse CI next to Frostfall - the tools coexist cleanly.
Both run axe-core; Lighthouse enables a curated subset of its rules (including
some best-practice rules), while Frostfall runs the full ruleset for the WCAG
standard you configure, with per-rule control on top.

## What this does and doesn't cover

Frostfall automates the automatable part of accessibility conformance. axe-core
is the same engine behind Lighthouse's accessibility score and most a11y tooling,
and running it against real rendered states - including keyboard-driven ones -
catches far more than static analysis. But no automated tool verifies that alt
text is meaningful, that a screen reader announces things sensibly, or that a
human can actually complete your flows with assistive technology. Use Frostfall
to catch regressions continuously; keep manual testing for what it's uniquely
good at.

## About Aquia

Frostfall is built and maintained by [Aquia](https://www.aquia.us), a
service-disabled Veteran-owned small business (SDVOSB) that has worked on
security and modernization for federal agencies and state governments since
2021. Accessibility is a daily reality in that mission space: Frostfall grew
out of Section 508 compliance work on production federal systems - the same
team that turned CISA's zero trust maturity model into a working scoring
application for the Department of Health and Human Services - and its sibling
tool [Emberfall](https://github.com/aquia-inc/emberfall) covers the API side
of the same testing story.

## License

Apache-2.0
