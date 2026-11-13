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
  keyboard: true                 # keyboard-audit every test's initial state,
                                 # discovered ones included (default off;
                                 # a test opts out with keyboard: false)
  expect:                        # ABSENT = report-only, always exit 0
    severity: serious            # fail on new violations at this impact or worse:
                                 #   minor | moderate | serious | critical
    maxViolations: 0             # tolerated count of failing violations (default 0)
    rules: [image-alt]           # also fail on these rule ids at ANY impact

auth:                            # optional: sign in before testing (see Auth below)
  setup:                         # a login flow, run through the normal steps
    path: /login
    steps:
      - fill: { "#email": "${A11Y_USER}" }
      - fill: { "#password": "${A11Y_PASS}" }
      - click: "button[type=submit]"
      - waitFor: "[data-testid=account-menu]"
  signedIn: "[data-testid=account-menu]"   # REQUIRED: a selector present on every
                                 # signed-in page and on no login page
  reuse: true                    # log in once, restore into every test (default)
  # storageState: .auth/state.json   # or: a Playwright storage-state file
  # loginPath: /login            # optional, no default: the login URL, for clearer
                               # messages and so discovery skips it
  # allowedOrigins: ["https://login.example.com"]   # SSO providers a setup flow
                               # passes through (never excuses a test)

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
    path: /                      # joined to the server's base URL (a leading / is
                                 # added unless the path starts with /, ? or #)
    # url: https://example.com/  # or an absolute URL (exactly one of path/url)
    scan: true                   # scan the initial state after load (default true)
    waitFor: "[data-testid=app]" # per-test readiness override
    rules:                       # per-test rule toggles (merged over defaults)
      color-contrast: off
    keyboard: true               # also run a keyboard audit on the initial
                                 # state (default false; see Keyboard audit)
    transcript: true             # also record a screen reader transcript of
                                 # the initial state (default false)
    auth: false                  # run without the configured auth: no session,
                                 # no login guard (public pages, signed-out states)
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
| `keyboard` | `keyboard: { label: modal, scope: "[role=dialog]" }` | run a [keyboard audit](#keyboard-audit) now; `label` and `scope` as for `scan`, plus `maxTabs` and `rules` |
| `transcript` | `transcript: { label: modal, scope: "[role=dialog]" }` | record what a [screen reader would announce](#screen-reader-transcripts-and-focus-assertions) now; `label` and `scope` as for `scan` |
| `expectFocus` | `expectFocus: { within: "[role=dialog]", name: Close }` | [assert where focus is](#screen-reader-transcripts-and-focus-assertions): `within` a selector, with accessible `role` and `name`; at least one, all must hold |

A step that fails (selector never appears within the test timeout) is an
environment error (exit 3), not a violation.

If a page's results are flaky, replace `waitFor: networkIdle` with a CSS
selector that only exists when the page is genuinely ready - that is almost
always the fix.

### Keyboard audit

axe inspects the DOM; it cannot press keys. A keyboard audit does: it presses
Tab through the page until focus cycles back to where it started, then
reports what a keyboard user would hit. It is opt-in, so upgrading Frostfall
never fails a pipeline on findings nobody asked for. Turn it on for every
test, discovered ones included, with `defaults: { keyboard: true }` (a test
opts out with `keyboard: false`, and a test's own `keyboard` settings layer
over the default's `maxTabs` and `rules`); per test with `keyboard: true` (audits the
initial state); or with a `keyboard` step (audits the state the steps
reached - an open dialog, say):

```yaml
  - id: checkout
    path: /cart
    keyboard: true
    steps:
      - click: "#open-checkout"
      - waitFor: "[role=dialog]"
      - keyboard: { label: modal-open, scope: "[role=dialog]" }
```

| rule | impact | WCAG | fails when |
|------|--------|------|------------|
| `frostfall/keyboard-trap` | critical | 2.1.2 | Tab stops moving focus, or cycles through part of the page and never returns to where it began |
| `frostfall/focus-unreachable` | serious | 2.1.1 | a visible element in the tab order is never reached by Tab |
| `frostfall/focus-not-visible` | serious | 2.4.7 | nothing visibly changes around an element when it gains focus |
| `frostfall/focus-hidden` | serious | 2.4.3, 2.4.7 | focus lands on an element that is 1px or smaller, clipped, or off-screen |
| `frostfall/dialog-focus` | serious | 2.4.3 | a modal dialog is open but focus is outside it, or Tab moves focus out of it |

Findings are ordinary violations: they fingerprint, baseline, gate on
`expect`, and report to SARIF and the HTML report exactly like axe rules, and
toggle the same way (`rules: { frostfall/focus-not-visible: off }`). `scope`
limits which elements can be reported; the traversal still covers the whole
page, since focus order is a property of the whole page.

How the checks decide:

- **Visible focus** is judged from screenshots: each stop's region (the
  element, plus close-fitting wrappers where design systems draw input rings)
  is captured while focused and again after Tab moves on. A region that did not
  visibly change is re-checked on a second lap with a longer wait before it is
  reported, so focus effects that mount late (MUI's ripple inside a DataGrid
  row) are not mistaken for missing ones. Changes too faint to see - under
  about 5% in every channel - do not count as an indicator.
- **Modal dialogs** are `aria-modal="true"` elements and open modal
  `<dialog>`s, or a portal whose page siblings are all `aria-hidden`/`inert`
  (how MUI and Radix modals wall off the page). While one is open, only its
  contents count as reachable.
- **Focus guards** - the zero-size `tabindex="0"` sentinels focus traps use -
  are not expected to be reached.

An audit presses Tab once per stop, at roughly 150ms each, inside the test's
`timeout`: a page with 150 tab stops needs about 25 seconds, so raise
`defaults.timeout` (or a profile's) when auditing large pages. It leaves focus and scroll where
it found them, so steps after a `keyboard` step run against the state the
config reached. Returning focus to a dialog's trigger when it closes is
checked with [`expectFocus`](#screen-reader-transcripts-and-focus-assertions).

### Screen reader transcripts and focus assertions

A `transcript` records what a screen reader would announce, in reading order,
read from the browser's accessibility tree - the same roles, names, and
states JAWS, NVDA, and VoiceOver get:

```text
dialog "Answer history" (modal)
  heading "Answer history" (level 2)
  button "Close"
  checkbox "Remember me" (checked)
  button (no accessible name)
```

Transcripts are evidence for a reviewer, never pass/fail: they appear in the
text output, as collapsible sections in the HTML report, and in the SARIF
run's `properties.transcripts`. They are not a screen reader - no speech
rules or verbosity settings - so keep testing with a real one; they make it
quick to spot a missing name or a wrong state without launching one. A
transcript stops at 2000 lines; `scope` it to the part of the page under
review.

`expectFocus` turns focus management into a regression test. Each field set
must hold: `within` (focus is inside this selector), `role` and `name` (the
focused element's accessible role and name, as the transcript shows them).
A miss is a `frostfall/focus-expectation` violation (serious, WCAG 2.4.3)
that gates and baselines like any other, with a message saying where focus
actually is:

```yaml
  - id: delete-dialog
    path: /items
    scan: false
    steps:
      - click: "#delete"
      - waitFor: "[role=dialog]"
      - expectFocus: { label: open, within: "[role=dialog]" }   # focus moved in
      - transcript: { label: open, scope: "[role=dialog]" }
      - press: Escape
      - expectFocus: { label: closed, name: Delete }            # and came back
```

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
| 2 | invalid config, including an `auth` block that is wrong (no `signedIn`, both `setup` and `storageState`, a missing or malformed storage-state file, a `url:` test off the app's origin without `auth: false`) and an unset `${VAR}` |
| 3 | environment failure (browser, server unreachable, page load, step failure), including the auth guard (a test landed on the login page, another origin, or without the `signedIn` marker), the startup probe, and a failed login flow |

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

## Auth: scan pages behind a login

Most real apps put their interesting pages behind a login. Frostfall signs in
once and starts every test with that session. The smallest config that works:

```yaml
auth:
  setup:
    path: /login
    steps:
      - fill: { "#email": "${A11Y_USER}" }
      - fill: { "#password": "${A11Y_PASS}" }
      - click: "button[type=submit]"
      - waitFor: "[data-testid=account-menu]"
  signedIn: "[data-testid=account-menu]"   # on every signed-in page, on no login page
```

Run it with `A11Y_USER` and `A11Y_PASS` in the environment (an unset `${VAR}` fails
the run with exit 2, which matters for pull requests from forks, where secrets are
not passed). The rest of this section explains each part. A config sets exactly
one of `setup` and `storageState` (both or neither is exit 2), and always sets
`signedIn`.

**`signedIn` (required).** A selector for something that exists on every
signed-in page and on no login page, such as `[data-testid=account-menu]`. After
each test's first navigation settles, Frostfall waits for it; if it is missing
the test fails as an environment error (exit 3) instead of being scanned. It is
required because a URL alone cannot tell a login page from the app: a login can
be rendered in place at the same URL, an identity provider's page lives on
another origin, and a route can be renamed. A marker in the page can. It must
be present on every page your tests load while signed in (use `auth: false` for
the ones that are public, below). The wait is up to 5 seconds, or a quarter of
`defaults.timeout` when that is under 20 seconds, so a missing session costs that
long, not the whole test timeout.

*Choosing it.* Pick a test id or element that only the signed-in app renders,
such as the account menu (`[data-testid=account-menu]`) or a piece of content
that needs the user. Do not use `main`, `nav`, `header` or a layout shell: those
render before authentication resolves, on the login page too, and the guard
would pass a login form. Frostfall checks your choice at startup; see
[the startup probe](#the-startup-probe).

**A login flow (`setup`).** Frostfall opens `setup.path`, runs the steps, and
captures what the login left behind: every cookie of the browser context
(`HttpOnly` ones included) plus the app origin's `localStorage` and
`sessionStorage`. Each test's fresh context gets that state before its first
navigation, so the page sees a signed-in session from its first request.

```yaml
auth:
  setup:
    path: /login
    steps:
      - fill: { "#email": "${A11Y_USER}" }
      - fill: { "#password": "${A11Y_PASS}" }
      - click: "button[type=submit]"
      - waitFor: "[data-testid=account-menu]"
  signedIn: "[data-testid=account-menu]"
  reuse: true      # default
```

A selector `defaults.waitFor` is ignored throughout setup (the first load and
every `goto` step use `load` instead), since it is usually something only
signed-in pages render. To wait for a page to hydrate, start the steps with an
explicit `- waitFor: "<selector>"`.

Setup steps are the interaction steps only (`click`, `fill`, `press`, `hover`,
`select`, `waitFor`, `wait`, `goto`). `scan`, `keyboard`, `transcript` and
`expectFocus` are rejected there. End the steps with a `waitFor` for something
only the signed-in app shows, so the capture happens after the login finished.

Frostfall checks that the flow worked: it must end on the app's origin, not on
the login page, with `signedIn` present. Otherwise setup fails with exit 3
instead of handing every test an empty session. A flow that finishes but leaves
no cookies or storage behind prints a `warning:` line even without `--verbose`.

With `reuse: true` the flow runs once, before the first test (and before
`--discover` crawls), and the state is restored into each test. With
`reuse: false` each test runs the flow itself inside its own context before
loading its page. That is slower and logs in once per test, so use it only
when the session cannot be shared (single-use tokens, per-session server
state). `--discover` honors it too: it logs in once per page it loads, up to
`discover.maxPages` loads, and each discovered test logs in again when it runs;
a warning at the start of the crawl says so. `reuse` only applies to `setup`.

**A storage-state file (`storageState`).** Point at a Playwright-compatible
storage-state JSON (`cookies` plus `origins[].localStorage`) captured
elsewhere, and Frostfall injects it the same way and skips the flow. This is
the path for CI when credentials should not be near this config. A relative
path resolves against the config file's directory. The file is read when the
config loads, so a missing or malformed one is a config error, and
`frostfall --validate` needs it to exist: write it first (see below). Cookies in
the file that have already expired are not sent; the browser would drop them
anyway, and the expired session then shows up as the guard below.

```yaml
auth:
  storageState: .auth/state.json
  signedIn: "[data-testid=account-menu]"
  loginPath: /login   # optional
```

To produce the file with Playwright, log in once in a script and save the
context's state:

```js
await context.storageState({ path: '.auth/state.json' });
```

Playwright writes `cookies` and `origins[].localStorage`, which is what Frostfall
reads (it does not read `sessionStorage` or IndexedDB from the file).

**The guard.** On the app's origin, a test fails with exit 3 when `signedIn` is
absent. The login route is a second signal and gives a clearer message:

```
run error: test "dashboard": landed on the login page /login: the auth session was not applied or has expired
```

- **`loginPath` (optional, no default).** The path, relative to the base URL,
  that a signed-out visitor lands on. Landing there fails the test with a
  clearer message, and `--discover` never follows it. `setup.path` is not a
  default: it is often a protected page that bounces through your identity
  provider and back, which is not a login page. With a base URL of
  `http://host/app` a `loginPath` of `/login` means `/app/login`; a trailing
  slash on the base changes nothing. Hash-router routes count as part of the
  path, the same way discovery treats them, so `loginPath: "/#/login"` guards a
  HashRouter app, and a query on the hash route (`/#/login?redirect=/x`) is still
  the login route. If your login is rendered in place at the same URL as the
  app, leave it out: `signedIn` alone tells them apart.
- **No exemptions.** Every guarded test is checked the same way, including one
  whose own path is the login page. To test the login page itself, or any
  signed-out state, set `auth: false` on that test.
- **Other origins.** A test on the app's origin that ends up on any other origin
  was bounced somewhere, usually an identity provider, and fails as a redirect.
  `allowedOrigins` does not change that. The session's cookies are sent to every
  host a test loads, so with auth configured a test whose `url:` is not on the
  base URL's origin (`localhost` is not `127.0.0.1`) is a config error (exit 2)
  unless it sets `auth: false`. Only the tests a run selects (`--id`, `--path`)
  are checked. `--validate` runs the check when the base URL is known without
  starting anything (`server.baseUrl` or `--base-url`); with `serve:` the origin
  is an ephemeral port, so the check runs at scan time.
- **Later in the test.** The guard runs again before and after every `scan`,
  `keyboard`, `transcript` and `expectFocus` step, so a `goto` or click that
  walks into the login page, or a page that redirects while it is being
  checked, fails there, rather than reporting the login page's results. The
  check looks at where the page is once it is ready; an app that redirects a
  few seconds later is not caught between checks, so use a `waitFor` selector
  for those.

**Public pages and the login page: `auth: false`.** A test with `auth: false`
gets a fresh context with no session and no guard, for a public page, a
signed-out state, the login page itself, or a page on another origin. It is a
no-op without an `auth` block, and `auth: true` without one is a config error:

```yaml
tests:
  - id: pricing
    path: /pricing
    auth: false
```

Discovered tests use the configured auth. A page `--discover` finds that is not
signed in (it lands on the login page or another origin, or lacks the `signedIn`
marker) is dropped with a log line, and the crawl ends with `discover: dropped N
of M pages (not signed in)`, printed even when N is 0. The cause is one of two:
a public page, which you add as an explicit test with `auth: false`, or a
signed-in page that does not render the marker, in which case pick a marker
every signed-in page renders.

**SSO and `allowedOrigins`.** If sign-in goes through an identity provider on
another origin (Okta, Entra, Keycloak), list that origin so the setup flow may
pass through it, and so storage the provider keeps for the session is captured.
The flow must still finish back on the app, with `signedIn` present. An
Okta-style setup (the selectors are illustrative; use your provider's):

```yaml
auth:
  setup:
    path: /login
    steps:
      - click: "#sign-in-with-okta"                       # leaves the app for the provider
      - fill: { "#okta-signin-username": "${A11Y_USER}" }  # on the provider's origin
      - fill: { "#okta-signin-password": "${A11Y_PASS}" }
      - click: "#okta-signin-submit"
      - waitFor: "[data-testid=account-menu]"                # back on the app
  allowedOrigins: ["https://example.okta.com"]
  signedIn: "[data-testid=account-menu]"
```

Entries are absolute `http(s)` origins (no path); the host is lower-cased and a
default port dropped. Frostfall captures the storage of the base origin and of
every allowed origin the flow visited. A setup that ends anywhere but the app's
origin fails with a message saying where it ended, which also catches a base URL
that redirects (to `https`, or to `www`): set `baseUrl` to the app's real origin.

**Timeouts.** With `reuse: false` the login flow has its own `defaults.timeout`
budget and the test's budget starts after it, so a slow login does not eat
into the test. The same holds for `--discover`, whose pages get 15 seconds
each apart from any login.

**Secrets.** Put credentials in environment variables and reference them as
`${A11Y_PASS}`. Frostfall prints a warning to stderr (it does not fail) when a
`fill` into a field whose selector contains `password`, `passwd` or
`type=password` has a literal value in the file. Setup steps are logged under
`--verbose` as the action and selector only, never the value, and error
messages never include a filled value. With auth configured, URLs in errors, logs and
reports (the report's Target line, and each result's page URL) are printed without
userinfo, query string, `;params` or fragment (a `#/hash-route` is kept), and a
path segment that looks like a secret is shown as `[redacted]`, so
`--base-url http://user:pw@host` and `setup.path: /login?token=...` cannot leak.
A path of your own is otherwise printed as written. The attribute values in axe's
selectors are scrubbed the same way before they are fingerprinted or reported: a
`a[href="/dl?token=..."]` becomes `a[href="/dl"]`. Without auth none of this
changes. The captured cookies and storage live only in memory for the run: they
are never written to disk, reports or logs. A `storageState` file is a
credential in itself; keep it out of version control (for example `.auth/` in
`.gitignore`).

**Screenshots and reports show signed-in pages.** The cookies and storage are
never written out, but what the signed-in pages display can be: element
screenshots of violations, the HTML report, and transcripts may contain account
data (names, emails, records), and a transcript records the visible values of
form fields. Screenshots are on by default under GitHub
Actions and written to `frostfall-artifacts/`, which you may then upload as a
build artifact. For a sensitive account, pass `--no-screenshots`, use a test
account with synthetic data, or do not upload the artifacts.

A failed login (a step times out, a selector is missing) stops the run with
exit 3 and a message naming the step: `auth setup: steps[1]: fill "#email": ...`.

**Credentials in CI.** The GitHub Action runs the binary in your job, so it
reads whatever environment the job has. Map secrets to variables at job level
(or workflow level) and reference them with `${...}` in the config:

```yaml
jobs:
  a11y:
    runs-on: ubuntu-latest
    env:
      A11Y_USER: ${{ secrets.A11Y_USER }}
      A11Y_PASS: ${{ secrets.A11Y_PASS }}
    steps:
      - uses: actions/checkout@v4
      - uses: aquia-inc/frostfall@v1
        with:
          base-url: https://staging.example.com
```

With the Docker image, pass the variables by name so the values stay out of the
command line and shell history:

```bash
docker run --rm --user "$(id -u):$(id -g)" -e A11Y_USER -e A11Y_PASS \
  -v "$PWD:/work" ghcr.io/aquia-inc/frostfall:v1 --base-url https://staging.example.com
```

To use a `storageState` from a CI secret, store the file's JSON in the secret
and write it to disk before the scan (and before anything that loads the config,
`--validate` included), through `env` (never interpolated into the script):

```yaml
- name: Write storage state
  env:
    FROSTFALL_STATE: ${{ secrets.FROSTFALL_STORAGE_STATE }}
  run: |
    mkdir -p .auth
    printf '%s' "$FROSTFALL_STATE" > .auth/state.json
    chmod 600 .auth/state.json
- uses: aquia-inc/frostfall@v1
```

If the secret holds the file base64-encoded, use `printf '%s' "$FROSTFALL_STATE" | base64 -d > .auth/state.json`.
For the Docker image, put the file under the directory you mount at `/work`;
the path in the config resolves against the config file as usual.

`--discover` crawls with the same session, so it sees the app and not the
login page, and it never follows what would end that session: with auth on it
skips paths that look like a logout (`/logout`, `/sign-out`, `/log_off`,
`/signoff`, matched case-insensitively) and the login page itself, on top of
your `discover.exclude`. A page that turns out to be the login page, another
origin, or unmarked when the crawl loads it is dropped rather than recorded as a
page. If the root page itself does that, the crawl fails the same way a test
does. Discovered paths become test ids in reports, baselines and issues, so with
auth on they are cleaned first, before your excludes and the login page exclusion
see them: a `?` in the path and `;params` are cut, and a hash route's query and
any nested fragment are dropped (`/#/home#access_token=...` becomes `/#/home`).
A link is skipped when its path still names a credential (`token=`,
`access_token`, `sig=`, `code=`, `password=`, `passwd=`, `key=`, `api_key`,
`apikey`, `session=`, `sessionid`, `ticket=`, `signature`, `x-amz-`, `auth=`,
`otp=`, matched case-insensitively, so a page called `/signature-pad` is skipped
too and goes in as an explicit test) or has a segment that looks like a secret: a
JWT (three base64url parts joined by dots), 32 or more hex characters, or 24 or
more base64url characters that carry a digit or mixed case and no recognizable
word (a slug like `release-notes-2024-summary` has words and is kept). The check
is a heuristic: it can miss a short or all-lowercase token, and it also skips
subtrees of opaque ids (UUIDs, ObjectIds), which is why the crawl logs how many
links it skipped (`discover: skipped N links that looked like they carry
credentials`). Without auth discovery output is unchanged.

`discover.maxPages` caps the pages scanned. Signed out that is the pages found.
Signed in it caps page loads, dropped pages included, since each load can cost a
login and a crawl whose pages keep being dropped must still stop.

### The startup probe

Before anything is scanned, Frostfall loads these pages in fresh contexts with no
session and no storage: the base URL, `loginPath` (when set), the setup page (in
setup mode), and the first explicit test that uses auth. They load in parallel
with `load` readiness whatever `defaults.waitFor` is (waiting for the network to
go idle would run out the timeout on an identity provider's page that keeps a
request open), and the marker is waited for the same way the guard waits for it.
If `signedIn` matches on any of them, the run stops with exit 3 and says which
page.

Two things print a `warning:` instead of failing: a probed page that lands off
the app's origin (the root bouncing to your identity provider, say), where the
marker could not be checked, shown as one line however many pages bounced; and,
if no probed page loaded on the app's origin at all, that the marker could not be
verified as absent when signed out. A page that lands off the app's origin and
then fails to settle is still just that warning. A base URL that cannot be loaded
warns, since a run that never loads it does not need it; any other probed page on
the app that fails to load stops the run. When every selected test says
`auth: false` and `--discover` is off, there is nothing to sign in for, and the
probe and the login flow are skipped.

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

With `auth` configured the crawl is signed in and skips logout-like paths and
the login page (see Auth above).

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

To scan pages behind a login, see [Auth](#auth-scan-pages-behind-a-login) for the
minimal config and for passing credentials to the action.

```yaml
- uses: aquia-inc/frostfall@v1
  id: frostfall
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
- if: steps.frostfall.outputs.new-violations != '0'
  run: echo "::warning::${{ steps.frostfall.outputs.new-violations }} new accessibility violations"
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
has first-class score budgets. If your need is a performance-plus-accessibility score
trend, run Lighthouse CI next to Frostfall - the tools coexist cleanly.
Both run axe-core; Lighthouse enables a curated subset of its rules (including
some best-practice rules), while Frostfall runs the full ruleset for the WCAG
standard you configure, with per-rule control on top.

## What this does and doesn't cover

Frostfall automates the automatable part of accessibility conformance. axe-core
is the same engine behind Lighthouse's accessibility score and most accessibility tooling,
and running it against real rendered states - including keyboard-driven ones -
catches far more than static analysis; the keyboard audit adds the tab-order
and focus checks axe cannot make. But no automated tool verifies that alt
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
