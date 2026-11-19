# Frostfall — Design Document

Accessibility test suite for rendered web apps. Sibling to [Emberfall](https://github.com/aquia-inc/emberfall):
Emberfall smoke-tests HTTP APIs with declarative YAML; Frostfall audits the rendered DOM of real
frontends (Vite, Next.js, CRA, anything that ships to a browser) for WCAG violations, locally and in CI.

Status: MVP implemented. The sections marked **(frozen)** are the expensive-to-change
contracts — config schema, violation fingerprinting, engine interface, exit codes, action interface.
Everything else is guidance the implementer may adapt.

Not yet implemented (config for these is rejected or flagged, never silently ignored):
server spawn mode (`server.command`), `--watch`, `--parallel`, and the
json/junit formatters.

---

## 1. Goals and non-goals

Goals:

- Single static Go binary. No Node, no npm install, no Docker required to run.
- Declarative YAML config in the Emberfall style: a list of tests, an `expect` block, sane defaults.
- Scans real rendered DOM, including interaction states (modals, dropdowns, post-validation forms),
  not just initial page loads.
- Works in three server modes without the user thinking about it: scan a running dev server,
  spawn one, or serve a static build directory itself.
- Adoptable on day one in a codebase with hundreds of existing violations, via a baseline file with
  "accept current, fail on new" semantics.
- First-class CI output: SARIF for GitHub code scanning, JUnit for test dashboards, JSON for machines,
  human text for terminals, `$GITHUB_STEP_SUMMARY` for the checks tab.
- Distributed exactly like Emberfall: Homebrew tap, `go install`, GitHub Action — plus the action is
  version-tagged (`@v1`), not `@main`.

Non-goals (v1):

- Not a general E2E test framework. Steps exist to reach states worth scanning, not to assert app logic.
- Not a performance tool. No Lighthouse scoring in v1 (the engine interface leaves the door open).
- No visual diffing. (Evidence screenshots are in scope: element-cropped PNGs of new violations,
  captured at scan time via `--screenshots`, on by default in GitHub Actions. Named by fingerprint
  in `frostfall-artifacts/`, referenced from reports. Diffing images run-over-run is not.)
- No multi-browser matrix. Chromium only in v1; the axe results are DOM-based and rarely differ by engine.

## 2. Architecture

```
┌────────────────────────────────────────────────────────┐
│ frostfall (Go binary)                                  │
│                                                        │
│  config loader ─▶ server manager ─▶ runner             │
│                    (running|spawn|serve)               │
│                                       │ per test       │
│                                       ▼                │
│                    browser session (go-rod / CDP)      │
│                       │ inject + run                   │
│                       ▼                                │
│                    Engine (axe-core, go:embed)         │
│                       │ raw violations                 │
│                       ▼                                │
│  fingerprinter ─▶ baseline filter ─▶ formatters        │
│                                      (text|json|sarif| │
│                                       junit|summary)   │
└────────────────────────────────────────────────────────┘
```

Key decisions, carried over from the brainstorm:

- **Browser driver: [go-rod](https://github.com/go-rod/rod).** Its launcher auto-downloads a managed
  Chromium when none is found (solves local UX), and detects system Chrome when present (GitHub
  runners have it preinstalled — zero download in CI). `--browser-path` overrides detection.
- **Scanner: axe-core, embedded.** `go:embed` the pinned `axe.min.js` (~600 KB), inject per page,
  call `axe.run()`, unmarshal JSON. Lighthouse's accessibility category is axe underneath, so going
  direct loses nothing on a11y and drops the Node dependency entirely. The axe version is baked into
  the binary and reported in output (results are only comparable across identical axe versions —
  see fingerprinting).
- **Engine interface from day one** even with a single implementation (§8).

## 3. CLI

```
frostfall [flags]

  --config PATH        config file (default: .frostfall.yml, then frostfall.yml)
  --base-url URL       running-server mode: scan an already-running server
  --serve DIR          static mode: serve DIR with the embedded file server
  --baseline PATH      baseline file (default: value from config, else none)
  --update-baseline    rewrite the baseline to match current results, exit 0
  --format F           text|json|sarif|junit (repeatable: --format sarif --format text)
  --output PATH        write the (first non-text) format here; text always goes to stdout
  --id REGEX           filter tests by id (Go regex, like Emberfall's --url/--method)
  --path REGEX         filter tests by path
  --watch              dev loop: rescan on change, print only newly introduced violations
  --discover           crawl same-origin from the root path, respecting discover config (§7.6)
  --dry-run            with --discover: print the discovered paths (one per line, sorted) and scan nothing
  --browser-path PATH  use this Chrome/Chromium instead of auto-detection
  --browser-url URL    attach to a running, signed-in Chrome (loopback DevTools address; needs
                       auth.attach: true; exclusive with --browser-path) (§7.5)
  --verbose            per-step logging
  --version / --help
```

Precedence: flags override config. `--base-url` and `--serve` each override the config's `server`
block entirely (running-server and static mode respectively).

### Exit codes (frozen)

| code | meaning |
|------|---------|
| 0 | ran to completion, no enforced failures (report-only mode always exits 0; violations may exist but are reported, baselined, or below the enforcement contract) |
| 1 | new violations breaking a configured `expect` contract |
| 2 | config invalid (schema error, unknown key, bad regex) |
| 3 | environment failure (browser launch, server never became ready, page load timeout, step failure; in attach mode, an endpoint nobody answers on, or a session that could not be read from the app's own page) |

Outside the contract, attach mode only (`--browser-url`, §7.5): an attached run interrupted by
SIGHUP, SIGINT, SIGQUIT or SIGTERM exits 129, 130, 131 or 143 after closing what it opened in the
user's browser, which is what a shell reports for a process killed by that signal. It is not a run
outcome, writes no report, and a run without `--browser-url` keeps the default signal behavior.

Emberfall collapses everything into 0/1; separating "your pages are broken" from "the tool couldn't
run" is what lets CI gate correctly and lets `continue-on-error` users tell the cases apart.

## 4. Config schema (frozen)

Unknown keys are a schema error (exit 2), not a warning — silent typos in a11y config mean silently
not scanning. Env interpolation `${VAR}` is supported in all string values; a reference to an unset
var is an error unless written `${VAR:-default}`.

```yaml
version: 1

# ── where the app comes from: exactly one of the three modes ──────────
server:
  # Mode 2 — Frostfall spawns it:
  command: yarn dev            # run via the shell; killed as a process group on exit
  port: 5173                   # port to probe and to build base URL from
  readyWhen: httpOk            # httpOk (default) | portOpen | logMatch
  readyPattern: "ready in"     # required iff readyWhen: logMatch (Go regex against combined output)
  readyPath: /                 # path polled for httpOk (default /)
  timeout: 60s
  env:
    NODE_ENV: test
  # Mode 3 — static build, mutually exclusive with command:
  # serve: ./dist
  # spaFallback: true          # serve index.html for unknown paths (SPA routing); default true
  # Mode 1 — running server, mutually exclusive with both:
  # baseUrl: http://localhost:5173

defaults:
  standard: wcag21aa           # wcag2a|wcag2aa|wcag21a|wcag21aa|wcag22aa|section508|best-practice
                               # each expands to axe's CUMULATIVE tag set (axe tags rules only with
                               # the level that introduced them); section508 = axe's section508 tag
                               # + WCAG 2.0 AA, which the 508 refresh incorporates by reference
  viewport: { width: 1280, height: 800 }
  waitFor: networkIdle         # per-page readiness: networkIdle | load | a CSS selector
  settleTime: 500ms            # extra settle after waitFor, absorbs late paints/suspense swaps
  timeout: 30s                 # per-test budget, load + steps + scans
  rules:                       # per-rule toggles applied to every scan
    color-contrast: on
  frames: true                 # scans reach into iframes (default true; see §7.4); a test's
                               # own frames wins, so this is how discovered tests opt out
  # Enforcement is OPT-IN: with no expect block anywhere, Frostfall is
  # report-only — violations are reported (terminal, SARIF annotations, step
  # summary) but the run always exits 0. Configuring expect turns on failing:
  expect:
    severity: serious          # fail at this axe impact or worse: minor|moderate|serious|critical
    maxViolations: 0           # max NEW (non-baselined) failures (default 0 once enforcing)
    rules: [image-alt]         # also fail on these rule ids at ANY impact —
                               # lets strict projects enforce a curated set first

auth:                          # optional; see §7.5
  setup:
    path: /login
    steps:
      - fill: { "#email": "${A11Y_USER}" }
      - fill: { "#password": "${A11Y_PASS}" }
      - click: "button[type=submit]"
      - waitFor: "[data-testid=account-menu]"
  reuse: true                  # persist cookies+storage once, restore per test (default true)
  # storageState: .auth/state.json   # alternative: inject pre-captured state, skip setup
  # attach: true               # alternative: the session of a Chrome the user signed in to, via
                               # --browser-url (local runs only)
  signedIn: "[data-testid=account-menu]"   # REQUIRED: present on every signed-in page, on no login page
  # loginPath: /login            # optional, no default: where a signed-out visitor lands, relative to
                               # the base URL
  # allowedOrigins: ["https://login.example.com"]   # SSO origins a setup flow may pass through

baseline: .frostfall-baseline.json

discover:                      # only used with --discover; see §7.6
  maxDepth: 3
  exclude: ["/admin/.*", "/logout"]
  maxPages: 100

tests:
  - id: home                   # required, unique; used in filtering, baseline entries, output
    path: /                    # joined to the base URL (config.JoinURL: the base's trailing slash is dropped for a /path
                               # and kept for ?query and #hash paths; a / is added unless the path
                               # starts with /, ? or #); absolute url: also accepted for cross-origin
    scan: true                 # scan the initial state after readiness (default true)
    frames: false              # keep this test's scans in the top document (default:
                               # defaults.frames, else true)

  - id: dashboard-direct       # fresh load of a deep route
    path: /dashboard
    waitFor: "[data-testid=chart-loaded]"   # per-test override of defaults.waitFor

  - id: dashboard-via-nav      # client-side navigation — different DOM, different bugs
    path: /
    scan: false                # skip the initial-state scan, only the step scans matter here
    steps:
      - click: "a[href='/dashboard']"
      - waitFor: networkIdle
      - scan: {}

  - id: checkout-modal
    path: /cart
    steps:
      - click: "#open-checkout"
      - waitFor: "[role=dialog]"
      - scan:
          label: modal-open    # names this scan point in output/baseline (default: step index)
          scope: "[role=dialog]"      # axe context: only scan within this subtree
          rules:
            color-contrast: off       # per-scan rule override
    expect:                    # per-test override of defaults.expect
      severity: moderate
```

### Step vocabulary (frozen)

Deliberately small — enough to reach a state, not to write E2E suites:

| step | behavior |
|------|----------|
| `click: SEL` | wait for selector visible, click |
| `fill: { SEL: value }` | focus, clear, type (fires input/change events) |
| `press: Key` | keyboard key to the focused element (`Tab`, `Enter`, `Escape`, …) — deliberately included: keyboard reachability bugs need keyboard steps |
| `hover: SEL` | mouse over (tooltip / menu states) |
| `select: { SEL: value }` | choose option in a `<select>` |
| `waitFor: X` | selector, `networkIdle`, or `load` |
| `wait: 500ms` | fixed sleep, escape hatch of last resort |
| `scan: {…}` | run the engine now; `label`, `scope`, `rules` as above; `scan` bare = all defaults |
| `goto: /path` | client-side-relevant fresh navigation mid-test |
| `transcript: {…}` | record the screen reader transcript (role, name, state in reading order, from the CDP accessibility tree); `label`, `scope`. A test-level `transcript: true` records the initial state. Evidence only, carried on the run, never a violation |
| `expectFocus: {…}` | assert focus is `within` a selector with an accessible `role`/`name`; a miss is a `frostfall/focus-expectation` violation |
| `keyboard: {…}` | press Tab through the page and report keyboard failures under `frostfall/` rule ids; `label`, `scope`, `maxTabs`, `rules`. A test-level `keyboard: true` audits the initial state; `defaults.keyboard` does so for every test, discovered ones included, and a test's own setting wins. Opt-in per config, never a binary default, so a release cannot fail enforcing pipelines on new findings. Additive: findings are `engine.Violation`s, so fingerprints, baselines, and formats are unchanged |

Any step failure (selector never appears within the test timeout, click on detached node) fails the
test with exit 3 semantics — it's an environment/spec failure, not a violation.

## 5. Violation fingerprinting (frozen)

The fingerprint decides what counts as "the same violation" across runs. Too specific and the
baseline churns on every DOM tweak (tool gets deleted); too loose and new violations hide behind old
ones (tool is useless). 

A violation's fingerprint is `sha256` (hex, first 16 bytes) of the following fields joined with `\x00`:

1. `testId` — from config
2. `scanLabel` — the scan point within the test (`initial` for the implicit initial scan)
3. `ruleId` — axe rule, e.g. `color-contrast`
4. `stableTarget` — a normalized selector for the offending node, computed as follows

`stableTarget` normalization, applied to axe's target selector for the node:

- If the node or an ancestor within 3 levels has an `id` that does not look generated, use
  `#id` + the relative path below it. "Looks generated" = matches `[0-9a-f]{8,}` or ends in a
  purely numeric suffix of 4+ digits (`:r1a:`-style React ids, CSS-modules hashes).
- Strip all class names matching hash-like patterns (`css-[a-z0-9]+`, `sc-[a-zA-Z]+`, `_[a-z0-9]{5,}$`,
  Tailwind arbitrary-value brackets excluded — plain utility classes are kept).
- Drop `:nth-child(n)` qualifiers when the element has any other distinguishing feature
  (id, surviving class, attribute); keep them only as a last resort.
- For a test that uses `auth:` (and only such a test), every attribute value in the selector is
  dropped before the steps above (`[href="/dl?token=x"]` becomes `[href]`; see §7.5). Elements that
  differ only in an attribute value then share a fingerprint, the same kind of collapsing as the
  generated-id and `:nth-child` rules, and a signed-in test's fingerprints differ from an
  unauthenticated run of the same page. A test with `auth: false`, and any config without `auth`,
  is unaffected, so the v1 scheme is unchanged for them.
- A target inside an iframe is frame-qualified: one selector per frame level, the top document
  first, joined with ` |> ` (`engine.FrameSeparator`). `stableTarget` splits it at the separator,
  counting only separators outside attribute brackets and quotes (an attribute value may contain
  the text ` |> `), normalizes each level with the steps above on its own, and joins the levels
  with ` |> ` again. An anchor id inside a frame therefore truncates only that frame's level,
  never the path to the frame, and the same node in two different frames stays two violations.
  This is an additive extension: a target with one level, which is every target in the top
  document, goes through the steps above exactly as before, so every existing fingerprint and
  baseline is unchanged for top-document targets. Before frames were scanned Frostfall never put
  axe into a frame, so multi-level targets did not normally occur. The one exception: a page that
  loads axe into its own frames let the plain `axe.run` test them, and those multi-level targets
  were joined with a space. They now join with ` |> ` (with `frames` on or off), so their
  fingerprints change once and `--update-baseline` realigns them. For a signed-in test the
  attribute values are dropped in every level, frame selectors included.
- (v2 candidate, NOT in the v1 scheme: preferring `data-testid`, then `role` + accessible name,
  over positional selectors. Changing this invalidates baselines, so it requires a fingerprint
  version bump with a migration path — not a silent amendment.)

What is deliberately **not** in the fingerprint: the failure summary text (axe rewords between
versions), the DOM snippet (churns), the impact level (axe recalibrates), the page URL beyond
`testId` (query params and trailing slashes would fork identities).

Consequence of using axe internals: the baseline file records the axe-core version. When the binary's
embedded axe version differs from the baseline's, Frostfall prints a prominent warning; enforcement
is otherwise unchanged, and `--update-baseline` realigns. (Planned: record the axe rule inventory in
the baseline so violations from rules that didn't exist in the baseline's axe version can be treated
as report-only until realignment — requires a baseline schema addition.)

## 6. Baseline file

`.frostfall-baseline.json`, checked into the repo, produced/refreshed only by `--update-baseline`:

```json
{
  "version": 1,
  "axeVersion": "4.10.2",
  "created": "2026-08-19T00:00:00Z",
  "violations": [
    {
      "fingerprint": "3f9a1c…",
      "testId": "dashboard-direct",
      "scanLabel": "initial",
      "ruleId": "color-contrast",
      "stableTarget": "#sidebar > nav [role=menuitem]",
      "note": "human context, optional, preserved across updates"
    }
  ]
}
```

Semantics:

- A current violation whose fingerprint appears in the baseline is **baselined**: reported in output
  (marked as such), never counted against `expect`.
- A baselined entry with no matching current violation is **stale**: reported as fixable debt in the
  summary ("3 baselined violations no longer occur — run --update-baseline"), never a failure.
  `--update-baseline` prunes them (preserving `note` on surviving entries).
- No baseline file + violations found = failure per `expect`. The getting-started docs lead with
  `frostfall --update-baseline` as step one of adoption.

## 7. Runtime behavior

### 7.1 Server modes

- **Running server** (`baseUrl` / `--base-url`): probe once for a non-5xx response before starting; if
  unreachable, exit 3 with a message that says which URL and suggests the other two modes.
- **Spawn** (`command`): start via shell in its own process group; on any exit path (including
  SIGINT) kill the group, not just the child — `yarn dev` wraps the real server. Readiness per
  `readyWhen`: `httpOk` polls `readyPath` every 250 ms for HTTP 200 with a non-empty body (port-open
  fires before Vite's first transform; a 200-with-body is the earliest honest signal); `logMatch`
  regexes combined stdout/stderr; `portOpen` is the lenient fallback. Server output is captured and
  replayed on failure, streamed live under `--verbose`.
- **Static** (`serve` / `--serve`): embedded file server on an ephemeral port, `spaFallback`
  rewriting unknown extensionless paths to `index.html`. This is the documented CI default: dev
  builds carry React error overlays, HMR wrappers, and dev-only DOM that produce phantom violations,
  and the production bundle is what users actually get.

### 7.2 Per-page readiness

`waitFor` + `settleTime`, per defaults with per-test/per-step override. `networkIdle` = no in-flight
requests for 500 ms (CDP `Network` domain). The selector form is the escape hatch for streaming App
Router pages and anything skeleton-shaped; the docs say plainly: *if results are flaky, use a selector.*

### 7.3 Test isolation and parallelism

Each test runs in a fresh browser context (cookies/storage from auth restored into it) — tests never
see each other's state. Contexts share one browser process. `--parallel N` (default: min(4, NumCPU))
runs tests concurrently; output order is stable (config order) regardless of completion order.

### 7.4 Scanning

Inject embedded axe if not present, then `axe.run(context, options)` with the tag set from
`standard`, per-scan `scope` as the context, and rule toggles merged (defaults → test → scan).
Marshal violations back over CDP. One scan's failure (axe throws, page navigated mid-scan) fails
that test with exit 3 semantics.

Iframes (`frames`, default on): the top document first asks axe for its frames in scope
(`axe.utils.getFrameContexts`). With none, the scan is the plain `axe.run` above, so a page without
frames gets exactly the results it always did. Otherwise the scan uses axe's partial-run API, the
flow `@axe-core/playwright` uses for drivers outside the page: `axe.runPartial` in the top
document, then depth first in each frame (inject axe, `getFrameContexts` and `runPartial` with the
same context and options, recurse), and finally `axe.finishRun(partials)` in the top document,
which merges them into one result whose targets carry the frame path (§5). A frame is reached
through its owner element: in the page's process with rod's frame page, or, for an out-of-process
frame (a cross-site frame under site isolation), by attaching to its CDP target. Which one is
decided from the owner element before anything runs in the frame. Each frame runs under a deadline
of its own (10s, cut short by the test's budget). A frame that cannot be reached or scanned in
time contributes one null partial, which `finishRun` skips together with the frames below it, and
a warning on stderr; the rest of the page is still scanned. The same happens to a frame removed
from the page mid-scan, and to a frame whose own frames changed between `getFrameContexts` and
`runPartial` (the partial's `frames` list no longer matches the list walked); if that happens in
the top document, the scan falls back to the plain `axe.run` and warns for every frame.

Coverage (what lets a baselined finding go stale, or its issue close) is counted per test across
its scan points. A scan that left a frame out counts as having run but not as having looked for
any axe rule. A full scan at another point of the same test still covers the rules, so a finding
in the skipped frame can still go stale from that test. A frame that is never reachable leaves
the test's axe rules uncovered altogether: nothing on that test's page, in the frame or out of it,
goes stale until the frame is reachable again or the test sets `frames: false`. `scope` stays a
top-document selector: a frame within it is scanned in full. Keyboard audits, transcripts and
`expectFocus` stay in the top document. In a signed-in test a finding inside a frame gets no
screenshot.

The launched browser keeps rod's default of site isolation off, so frames of every origin run in
the page's process; `browser.LaunchOptions.SiteIsolation` turns it on (tests use it to exercise the
out-of-process path). Under site isolation, `networkIdle` readiness does not see an
out-of-process frame's document finish loading and waits out the budget, which is one reason it
stays off.

### 7.5 Auth

Exactly one of `setup`, `storageState` and `attach` is set (more than one, or none, is exit 2),
and `signedIn` is always set. The whole block is validated at load: every mistake in it is a config error, because
silently ignoring auth means scanning a login page and passing. A `storageState` file is read at
load, so `--validate` needs it to exist.

**Profiles.** A profile may set `auth`, which replaces the top-level block wholesale (never merged:
setup from one and `storageState` from the other would be both). Validation runs on the resolved
block after the profile applies, with errors prefixed `profile "<name>":`, and everything
downstream (the guard, redaction, `RedactingAuth`, `CheckTestOrigins`) reads only the resolved
`Config.Auth`. An unselected profile's auth gets strict decoding only, like its other fields, so a
CI-only state file is not read locally. A profile cannot remove auth: a null or scalar `auth` in
any profile is a config error, since `auth: null` decodes like an absent key and would otherwise
be read as "inherit". The reason for the rule is that the `ci` profile is selected automatically
in GitHub Actions: a profile that dropped auth would silently turn off the signed-in guard and the
URL and selector redaction for every CI run. A test that should run signed out sets `auth: false`
on itself instead. The check walks the document, `profiles` and each profile through aliases and
merge keys (`<<`) with the decoder's precedence, and compares keys as the decoder decodes them, so
an alias key or a `!!binary` key that decodes to `auth` counts as `auth`. When `profiles` or the
document root uses a merge key, a profile name must resolve to a string: yaml.v3 records an unquoted
`2024`, `1.5`, `true`, date or null key in its own type but looks merged keys up as strings, so a
merged profile would replace an explicit one of the same name, and such names are a config error
that says to quote them. Without a merge key the quirk cannot arise and those names load as they
always have, so released configs keep working. The walk only produces the
error message. The safety comes from `applyProfile`, which inherits the top-level block whenever
the decoded profile auth is nil, so a removal the walk missed would keep auth, never drop it. The
walk expands each mapping once, so alias diamonds stay linear (it runs before the decoder's
excessive-aliasing check) and a self-merge ends there and is reported by the decoder. The
password lint reads every profile's `auth.setup.steps` as well as the top-level ones.

**`signedIn` is the guard's main signal.** A URL comparison cannot tell a signed-out page from a
signed-in one: a login can be rendered in place, an identity provider's page is on another origin, a
route can be renamed. A marker in the page can, so the selector is required and must exist on every
signed-in page and on no login page. `loginPath` is a second signal and the source of clearer
messages; it is optional and has no default (`setup.path` is often a protected page that bounces
through SSO and back, which is not a login page). The marker is verified before anything is
scanned (`Auth.probeSignedOut`): the base URL, `loginPath` (when set), the setup page and the first
explicit test that uses auth are loaded in parallel in fresh contexts with no session and no seed,
and a marker that matches on any of them fails the run (exit 3). The probe loads with `load` whatever
`defaults.waitFor` is (networkIdle would wait out an identity provider's open request), the landing
URL is read before anything can fail, and the marker is waited for with the guard's own wait (`has`,
`min(5s, defaults.timeout / 4)`), so a marker drawn a moment after load is seen. The setup flow's
first load and `goto` default also ignore a selector `defaults.waitFor`. A page that lands off the
app's origin cannot be checked: that is a warning (one line however many pages bounced), also when
the page then fails to settle; so is a run where no probed page landed on the app's origin, and an
unreachable base URL (a failure to load any other probed page on the app is fatal). Warnings
already collected are always printed. The probe and the login flow are skipped when
every selected test has `auth: false` and discovery is off; the run keeps only the redaction.

**Setup.** `setup.path` and a non-empty list of interaction steps (`scan`, `keyboard`, `transcript`
and `expectFocus` are rejected: setup has no result to report them to). With `reuse: true`
(default) the flow runs once, before the first test and before discovery, in a throwaway incognito
context. Everything it left behind is captured in memory. Storage first, then cookies, so the state
describes one moment: `localStorage` and `sessionStorage` of the app origin and of every allowed
origin (below) the flow visited (the page the flow ended on is read in place; a visited origin it
left is revisited at its root, and a revisit that fails prints a `warning:`), then every cookie of
that context (CDP `Storage.getCookies`, so `HttpOnly` ones too). Every test's context is
seeded before its first navigation: cookies through `Storage.setCookies` on the context, storage
through `Page.addScriptToEvaluateOnNewDocument`. Each storage block is guarded by
`location.origin === <origin>`, so a value never lands on another origin, and a key the page
already holds is left alone. The script is removed once the first load is ready: left registered it
would re-run on every later navigation and restore a token the page had deliberately removed (a
logout step). With `reuse: false` each test runs the flow inside its own context, before loading
its page; discovery honors that too (see Discovery below). `reuse` is
meaningless with `storageState` and rejected there.

**Setup success is checked.** After the steps the flow must be on the base origin, not on the login
route, with `signedIn` present; otherwise setup fails (exit 3). Ending on any other origin says where
it ended and that an SSO flow must finish back on the app, which is also what makes a base URL that
redirects (to https, to www) fail loudly instead of silently dropping storage. A flow that leaves no
cookies or storage behind warns on stderr regardless of `--verbose`.

**storageState.** A Playwright-compatible file (`cookies[]`, `origins[].localStorage[]`), the
CI-friendly path when credentials shouldn't be near this config. Relative paths resolve against the
config file's directory; a missing or malformed file is a config error (the error never echoes a
cookie or storage value). `expires: -1` maps to a session cookie and `sameSite` Strict/Lax/None to
the CDP enum. Cookies already expired are left out; Chrome silently drops an expired cookie from
`Storage.setCookies` without failing the batch, so skipping them changes no outcome.

**Attach.** `attach: true` takes the session from a Chrome the user started with
`--remote-debugging-port` (and, since Chrome 136, a non-default `--user-data-dir`) and signed in to
by hand, for logins that cannot be scripted (MFA, smart cards) and apps reachable only from inside
a network. It is a local workflow: the action has no input for it. The key and `--browser-url` are
paired strictly on the resolved block: the flag with any other block (or none) is exit 2, and so is
the key without the flag, except under `--validate`, which checks the config alone. `--discover
--dry-run` connects a browser and needs the flag like any run.

- *Endpoint.* `browser.ParseBrowserURL` accepts `http://` (resolved by reading `/json/version`
  with Frostfall's own client: loopback-only dialing, no redirect followed, a 64 KB body cap and
  one deadline, where go-rod's `launcher.ResolveURL` follows redirects to any host and has no
  bound; the answer must be a browser `ws://` URL, whose host is replaced with the given one) or a
  `ws://.../devtools/browser/...` URL, on a loopback host only (`localhost`, 127.0.0.0/8, ::1) with an explicit port; anything else
  is exit 2. The WebSocket dial refuses a connection whose remote address is not loopback, so a
  `localhost` that resolves elsewhere fails too. A `ws://` path is the browser's GUID, which is a
  control token: `Endpoint` prints as `scheme://host:port`, and connection errors are built from
  fixed phrases (a refused dial, a timeout), never from a `url.Error` or handshake error that would
  carry the URL. An endpoint that does not answer is exit 3 with a hint naming
  `--remote-debugging-port`, `--user-data-dir` (and that another Chrome already on that directory
  keeps the port from opening) and the `RemoteDebuggingAllowed` policy. `--browser-url=` (empty)
  is exit 2, and `Endpoint` formats with value receivers so a copied value prints the same.
- *Lifecycle.* The dial deadline stays on the socket through go-rod's first DevTools exchange, so a
  server that accepts the WebSocket and then stays silent fails within the connect timeout.
  `browser.Connect` owns the WebSocket, and `Browser.Close` in attach mode never sends
  `Browser.close` (that would quit the user's Chrome): it disposes the contexts and closes the tab
  Frostfall created that are still open, then drops the connection. Test contexts are created with
  `Target.createBrowserContext{disposeOnDetach: true}`, verified to make Chrome dispose them when the
  connection drops, so a crash or kill leaves no seeded context behind; a SIGINT or SIGTERM also
  runs `Close` before exiting (130 or 143). `Session.Close` refuses to close a handle with no context
  id, which would be the browser itself.
- *Capture* (`Auth.captureAttached`, the attach case of `NewAuth`, which sets `a.state` so seeding,
  the guard and redaction are unchanged). The user's tabs are never read, navigated or closed. One
  background tab in the default context does it all; it is created and registered under the
  registry lock that `Close` takes, so an interrupt cannot fall between the two. Chrome reads an
  origin's `localStorage` only through a frame of that origin (`DOMStorage.getDOMStorageItems`
  from about:blank answers "Frame not found for the given storage id", checked against Chrome
  154), and `Network.getCookies` from about:blank leaves out a first-party partitioned (CHIPS)
  cookie that it returns from a page on the origin. So the tab visits the app's base URL and each
  allowed origin's root, and nothing of that visit reaches the app:
  `Network.setBypassServiceWorker` is on for the tab (a registered worker would otherwise answer
  the navigation with the real app shell and fetch with the user's cookies, which a test
  reproduced), and every request is intercepted (`Fetch`) for the whole life of the tab: the
  document is fulfilled with an empty, untitled page and anything else failed. Fetch is never
  disabled while the tab is open, since disabling it releases a paused request to the network (a
  test reproduced that too); a request still paused at the end is aborted when the tab closes.
  The page checks `location.origin` itself before returning storage. Each visit's URL is the one
  Chrome writes, asked of Chrome's own parser (`new URL()` in the capture tab): the base path in its
  escaped form, an IDN host in punycode; paused requests are matched by parsed scheme, host and
  escaped path, never by string. (Built from the decoded path, `/100%25/` became a request the
  interceptor did not recognize, and the run stopped.) The empty page is answered with HTTP 404,
  which Chrome records as a hidden history row (`hidden=1`, so it is left out of `chrome://history`
  and the address bar's suggestions; checked on Chrome 154 and 157), and it has no title, so an
  existing row keeps its own title and stays visible (with a title, Chrome renamed it). The visit
  is a link and adds no typed count, with one exception that is Chrome's own: on a host with no
  public suffix (an IP address, an intranet name) that was never typed, the history backend records
  the first visit as typed so the omnibox learns the host (measured: `127.0.0.1` on a fresh profile
  gives `typed_count=1`, `app.example.com` gives 0, and a host with an earlier typed visit gives 0).
  With history sync on, the hidden visit is synced too; Chromium's history sync has no hidden or 404
  filter. An earlier version marked the navigation `auto_subframe` instead, which worked up to
  Chrome 154; Chrome 157 never commits a top-level navigation with that transition, so the visit
  timed out and the capture came back with no localStorage. A visit to the app's own origin that
  fails stops the run (exit 3) with the cause (a timeout, or Chrome's `net::ERR_*` code), instead of
  a warning followed by a scan from a partial session that the guard could only call expired; a
  failed visit to an allowed origin still warns. Before the tab closes it is navigated to
  about:blank and its navigation history reset, so the visited origins are in neither
  `Sessions/Session_*` nor `Sessions/Tabs_*` ("Recently closed" keeps only an about:blank entry). On
  each origin's page, `Network.getCookies` for that URL does the matching, so no other site's cookie
  enters memory, and `attachedCookies` checks again (RFC 6265 domain match including parent-domain
  cookies and bracketed IPv6 literals, the cookie's path covering the scope's, `Secure` only for
  https, a partitioned cookie only when its partition is the scope's own site). A cookie scoped to
  a deeper path than the URL asked for (Keycloak's `KEYCLOAK_IDENTITY` on `/realms/<realm>/`) is
  not returned and not carried; the paths are unknown, and reading the whole profile with
  `Storage.getCookies` to find them was ruled out. An origin whose storage cannot be read is
  skipped with a warning. `sessionStorage` is per tab and is not carried over. One stderr line
  always names the origins and the cookie and key counts; an empty capture also warns, and the
  guard then stops the run.
- *Headed browser.* The user's Chrome is headed and a test window is often not the focused one. An
  unfocused page reports `document.hasFocus()` false (measured: always, when another window keeps
  taking focus), and `FocusedAX` then returns no element, which produced a false
  `frostfall/focus-expectation` on a page that moves focus correctly in one run of four. Attach mode
  enables `Emulation.setFocusEmulationEnabled` on every test page, which made `hasFocus()` true in
  every case and removed that finding. The testdata keyboard and transcript fixtures gave the same
  findings headless, attached headless and attached headed, with or without focus being taken away.
  CI checks this with a headed Chrome under bare Xvfb (a new window took no focus under fluxbox on
  the runner); where xdotool is installed the test also gives the other window X input focus
  directly (`windowactivate`, then `windowfocus`, which needs no window manager), and a page
  without emulation must first read false, or the run proves nothing and fails.
  The attach connection also turns off go-rod's default device emulation, which would present an
  emulated user-agent instead of the one the user signed in with; the viewport is set per test.
- *Interrupts.* In attach mode SIGHUP, SIGINT, SIGQUIT and SIGTERM are caught: the handler sets
  an `interrupted` flag, runs `Close`, and exits 129, 130, 131 or 143 (128 plus the signal number,
  what a shell reports for a process killed by it; see §3). `Close` has one 5 second budget for
  everything it does (scrubbing and closing the tab, disposing contexts, waiting for the tab to
  go), so a browser that stops answering cannot hold the exit up; `waitGone` reports a timeout
  through `Browser.Warn`. A second signal while `Close` runs exits at once with the first's code. Once the flag is set the main
  goroutine's failure paths park instead of printing an error or writing a report, so the
  handler's exit is the outcome. As soon as the tests have run the browser is released and then
  the handler stopped; stopping honours a signal that arrived meanwhile (exit 128+n, no report),
  and after that an interrupt while reports are written behaves as in launch mode.
- *go-rod's own flags.* go-rod reads `-rod=...` (or `-rod <opts>`) from anywhere in `os.Args` in a
  package init, before `main`; its `cdp` option logs every CDP message, which in attach mode
  includes every cookie of the user's profile. `run` refuses any argument matching `^--?rod(=|$)`
  with exit 2 before anything connects. (An unknown option there makes go-rod panic during init,
  before Frostfall can say anything.)

**Landing check** (`Session.CheckLanding`). After each test's initial navigation and readiness, and
again before and after every `scan`, `keyboard`, `transcript` and `expectFocus` step (so a `goto`
into the login page, or a page that redirects during the check, is never reported on), a test fails
as an environment error (exit 3) when:

- the page is on any origin but the app's (`allowedOrigins` does not excuse it);
- the path plus hash route equals `loginPath` (resolved under the base URL's path: base
  `http://h/app` and `loginPath /login` is `/app/login`, a trailing slash on the base is ignored):
  `test "x": landed on the login page /login: the auth session was not applied or has expired`;
- or `signedIn` is absent after a short wait, `min(5s, defaults.timeout / 4)`, never the whole test
  budget.

There is no exemption. A test of the login page itself, a signed-out state, a public page or a page
on another origin sets `auth: false`. Path identity is discovery's (`RouteKey`: trailing slash
trimmed, query and plain fragments dropped, a `#/route` fragment kept) with one difference in the
guard only: the query of a hash route is dropped too, so `/#/login?redirect=/x` is the login path.

**Per-test `auth: false`.** A strict boolean on a test. That test gets a fresh context with no
injected session and no guard. Its URLs are still printed redacted, since redaction follows the run's
auth config (`Auth.Anonymous`). Discovered tests use the configured auth. `auth: true` with no
`auth` block is a config error; `auth: false` there is a no-op. With auth configured, a test whose
`url:` is not on the base URL's origin is a config error (exit 2) unless it sets `auth: false`: the
session's cookies are injected into the whole context, so they would go to a host the guard cannot
judge (`CheckTestOrigins`, over the tests the run selects with `--id`/`--path`). It runs under
`--validate` (which also compiles the `--id`/`--path` filters like a real run) when the base URL is known without starting a server (`server.baseUrl`, `--base-url`),
and at scan time otherwise (`serve:` binds an ephemeral port).

**Allowed origins.** `allowedOrigins` lists absolute http(s) origins (validated; `NormalizeOrigin`
lower-cases the host, writes an IDN host in punycode and drops a default port) that a *setup flow*
may pass through, an SSO identity provider: their storage is captured. They are never an exception
for a test. A browser reports `location.origin` lower-cased and in punycode, and capture, seed and
guard all compare against the normalized form, the base URL's origin included. Every origin goes
through one helper, `config.CanonicalHost`: an ASCII host is only lower-cased (and an IPv6 literal
bracketed), byte for byte as before IDN support; a non-ASCII host is converted with
`golang.org/x/net/idna` set up as the WHATWG URL standard's domain-to-ASCII (UTS #46,
non-transitional, bidi and joiner checks, no STD3 or hyphen checks). `idna.Lookup` is stricter than
that and would refuse hosts Chrome loads (an underscore or a leading hyphen in a label). On top of
the conversion it refuses a non-ASCII host when:
- a label starts with `xn--` once mapped (`XN--`, fullwidth `ｘｎ--`, a soft hyphen inside) but is not
  all ASCII. x/net/idna's punycode decoder takes the last hyphen of `xn--café-` for the delimiter and
  decodes `café`, so `xn--café-.example` would come out as the real `xn--caf-dma.example`. The labels
  are mapped with x/net/idna's own mapping (each prefixed with `a`, which keeps it from being decoded);
- the bidi rule fails on the mapped labels. x/net/idna decides whether to run the rule from the runes
  as written, so ℵ (U+2135, which maps to Hebrew alef) after a Latin letter skipped it; converting
  the result a second time decodes its punycode labels and applies the rule to them, and that pass
  must give back the same string;
- the result is empty, holds a forbidden domain code point (fullwidth ／, ＠ and ： map to `/`, `@`
  and `:`, so `：：1` is not `[::1]` and `evil.com＠app.example` is not userinfo), or ends in a number;
- an `xn--` label decodes to nothing. A label empty as written is kept, as Chrome keeps it.

A test compares every one of these rows with Chrome's own `new URL()`. The one deliberate difference:
a non-ASCII host whose last label is a number that Chrome rewrites to an IPv4 address
(`１２７.0.0.1`, `1.2.3.4。`) is refused rather than converted. These are the checks made, not a claim
to refuse all Chrome refuses: a host Chrome refuses for a reason not listed here is still converted,
and nothing proves the result is a host no page reports. The malformed `xn--` label was such a case
until it was checked; the Chrome comparison test is where any other belongs. A refused host is an
error in `allowedOrigins`, a storage-state origin, a test's `url:` (`CheckTestOrigins`), the base
URL's reachability probe (exit 2, before any request) and the base URL (`NewAuth`); anywhere else it
is kept exactly as written. It is non-ASCII, so it matches nothing the browser reports (lower-casing
it could not promise that: Go maps the Kelvin sign to `k` and `İ` to `i`). Discovery's same-origin
filter converts a non-ASCII host the same way, dropping its scheme's default port, and keeps its
plain host:port comparison for ASCII hosts. The base URL's reachability probe (`net/http`) requests
the punycode host, since Go's client would otherwise apply `idna.Lookup` and dial the raw UTF-8 host
for a label it refuses.

**Timeouts.** With `reuse: false` the flow runs under its own `defaults.timeout` and the test's
budget (or discovery's 15s per page) is restarted after it.

**Discovery.** The crawl uses the same seeding. With auth on it excludes logout-like paths
(`(?i)/(log|sign)[-_]?(out|off)\b`) and `loginPath` (when set), on top of `discover.exclude`; pages
at the depth limit are still loaded once, so that a page that lands on login, off the app's origin,
or without the marker is dropped, not recorded, and a root that does so fails the crawl. Each
drop logs a hint that covers both causes (a public page: add it as a test with `auth: false`; a
signed-in page without the marker: choose a marker every signed-in page renders), and the crawl always
ends with `discover: dropped N of M pages (not signed in)`. Signed out, depth-limit pages are not
loaded. With `reuse: false` the crawl logs in once per page loaded, up to `discover.maxPages` loads
(a session captured once would be dead after the first page for an app with single-use or rotating
sessions), each discovered test logs in again when it runs, and a stderr warning says so at the
start. `maxPages` caps pages found when signed out (unchanged) and page loads, dropped pages
included, when signed in. Discovered paths are test ids in reports, baselines and issues, so with auth
on they are cleaned before the excludes and the dedupe see them: a `?` in the path and `;params` are
cut, a hash route's query and any nested fragment are dropped, and a path is skipped when it still
names a credential (`credentialMarks`: `token=`, `access_token`, `sig=`, `code=`, `password=`,
`passwd=`, `key=`, `api_key`, `apikey`, `session=`, `sessionid`, `ticket=`, `signature`, `x-amz-`,
`auth=`, `otp=`, case-insensitive substrings) or has a segment that looks like a secret
(`tokenLike`: a JWT, 32+ hex characters, or 24+ base64url characters with a digit or mixed case and
no word between separators); a signed-in crawl logs how many links it skipped for that
(`discover: skipped N links that looked like they carry credentials`); without auth the output is
unchanged.

**Secrets.** Only through `${ENV}` interpolation. A `fill` into a password-looking selector
(`password`, `passwd`, `type=password`, case-insensitive) whose raw text has no `${...}` is a
warning on stderr (never an error, never echoing the value), checked against the text before
interpolation. Setup failures are exit 3 and name the step index (`auth setup: steps[1]: ...`).
Verbose logging of setup steps names the action and selector, never a value. With auth configured,
URLs in errors and logs go through `RedactURL`: userinfo, query, `;params` and fragment are dropped,
a percent-encoded `?` or `#` in the path is cut like a real one, and a hash route is kept only up to
the first `#`, `?`, `&` or `=` (OAuth implicit flows put the token there), so a token in
`setup.path` cannot leak; without auth the output is unchanged. With auth configured, `PageURL` in results and transcripts (SARIF locations, the HTML report) is
redacted the same way, as is the report header's base URL and the "cannot reach" message; a path
segment that looks like a secret (`secrets.TokenLike`) is masked as `[redacted]`. axe selectors of a test that uses auth are
scrubbed before they are fingerprinted or reported (`secrets.ScrubSelector`, a small parser, not a
pattern: every attribute value is dropped, `[href="/dl?token=x"]` becomes `[href]`, whatever the
attribute name, escaping or operator, and text outside attribute brackets is untouched); a test with
`auth: false`, and any config without auth, keeps its exact targets, so the fingerprint scheme is
unchanged for them. For a signed-in test, elements that differ only in an attribute value share a
fingerprint, as the existing collapsing of generated ids and `:nth-child` already merges
near-identical elements, and the fingerprints differ from an unauthenticated run of the same page.
`Violation.HTML` is never rendered or
stored by any formatter, the baseline or the issue code, so it is not scrubbed. The captured cookies and storage exist only in memory: never written to disk,
reports or logs, and the state type redacts itself under `%v`. That is about the session itself:
element screenshots, the HTML report and transcripts of signed-in pages can contain account data,
and screenshots default on under GitHub Actions, so sensitive accounts should run with
`--no-screenshots`. `Session.Close` disposes the incognito context so a signed-in context does not
outlive its test.

### 7.6 Discovery

`--discover` crawls same-origin `href`s from `/` (breadth-first, `maxDepth`, `exclude` regexes,
`maxPages`), deduplicates by path, and synthesizes fresh-load tests with generated ids
(`discovered:/pricing`). Two SPA realities learned from dogfooding: hash-router routes (`/#/users`)
keep the fragment as path identity rather than collapsing into `/`, and numeric path segments
dedupe by shape (`/items/1` and `/items/999` are one route, one representative scan). Discovered tests use `defaults` for everything. Explicit tests always win on
path collision, but only a test with a `path:` can: a test with an absolute `url:` never covers a
discovered page, so its page can still be discovered and scanned a second time. `--dry-run` lists the crawl's
paths without scanning and counts coverage the same way, against every test in the config
(`--id`/`--path` do not narrow it). Signed in, the listing fails closed at the print boundary
rather than trusting the crawl's cleaning: a path with any of `%`, `?`, `;`, `&`, `=`, or a `#` that
is not a single `#/` hash route, is skipped and counted on stderr, never printed. Decoding layer by
layer to look for a hidden credential was tried and has no stopping point (a stray `%` ends the
decode, and each new encoding needs a new check), while a character rule has nothing to decode. The
cost is that a signed-in `/100%` is skipped too; `//x` still prints. A plain secret in an ordinary
segment (`/a/s3cr3t`) carries none of those characters and is not caught, and neither is a
lookalike of `?` or `=` (fullwidth `？` `＝`, U+037E), which is plain text to the rule. Token-shaped
segments are masked on the printed line and on each test's `path:` before coverage is compared. A
path with an unprintable character (a control character, invalid UTF-8 such as a raw `%9B` CSI byte,
a format character such as a bidi override, or a line or paragraph separator) is skipped signed in
or out, since it would not print as the one line it is.
Discovery is additive to the config, not a replacement — the recommended shape is a
handful of explicit stateful tests plus discovery for coverage breadth. (Next.js routes-manifest
parsing is future work, §10.)

### 7.7 Watch mode

`--watch` implies running-server mode. Initial full run establishes the session's "seen" set (baseline +
first-run results); on file change under the watched dir (default: cwd) or on Enter, rescan and print
only violations not in the seen set. Fixed violations are announced. Exit code irrelevant; this is
the inner-loop tool that earns adoption, not a gate.

## 8. Engine interface (frozen)

```go
// Engine audits a page that the runner has already navigated to readiness.
type Engine interface {
    Name() string          // "axe"
    Version() string       // embedded engine version, e.g. "4.10.2"
    // Audit runs the engine in the given page with per-scan options and
    // returns normalized violations. It must not navigate the page.
    Audit(ctx context.Context, page Page, opts ScanOptions) ([]Violation, error)
}

type ScanOptions struct {
    Standard string            // tag set / conformance target
    Scope    string            // CSS selector context, "" = document
    Rules    map[string]bool   // rule id -> enabled
    Frames   bool              // also scan iframes, when the Page is a FramePage
    Warn     func(frame string, err error) // told about each frame left out
}

type Violation struct {
    RuleID   string   // engine-namespaced when not axe: "equal-access/…"
    Impact   Impact   // Minor | Moderate | Serious | Critical
    Target   string   // raw selector from the engine (fingerprinter normalizes it)
    Summary  string   // human description
    HelpURL  string
    HTML     string   // offending node snippet, for reports only
}

// Page is the minimal surface an engine needs, implemented by the rod session.
type Page interface {
    Eval(js string, args ...any) (json.RawMessage, error)
    InjectScript(src string) error
}

// FramePage is optional: a Page that can reach its child frames' documents.
// locate is a JS function, run in this page with arg, returning the frame's
// owner element; the frame's page runs under a deadline of its own.
type FramePage interface {
    Page
    Frame(locate string, arg json.RawMessage) (FramePage, error)
}
```

`Frames`, `Warn` and `FramePage` are additive: an engine or Page that ignores them scans the top
document as before. A frame-qualified `Violation.Target` joins its frame levels with
`engine.FrameSeparator` (` |> `).

Fingerprinting, baselining, and formatting all consume `[]Violation` and know nothing about axe.
That is the whole point: pa11y-style HTML_CodeSniffer, IBM Equal Access, or a Lighthouse-as-engine
adapter slot in behind this without touching anything downstream. v1 registers only `axe`.

## 9. Output

**text** (default, stdout): grouped by test → scan point, colorized, impact-sorted, baselined
violations collapsed to a count, summary block last (new / baselined / stale-baselined / tests run).

**json**: complete structured results — every violation with fingerprint, baselined flag, raw target,
snippet, help URL, plus run metadata (frostfall version, axe version, base URL, timing).

**sarif** (2.1.0): one rule per axe rule with `helpUri`; results locate at stable synthetic
repo-relative paths (`frostfall/<testId>`, startLine 1 — GitHub rejects absolute http URIs and
requires a region), the DOM selector in `logicalLocations`, and the page URL in the message. The
violation fingerprint is written to `primaryLocationLineHash`, the only key GitHub uses for alert
identity, so alerts track baseline identity exactly. Baselined violations are omitted (GitHub
ignores `baselineState`): known debt opens no alerts and baselining closes the alert on the next
upload. Alerts render in the Security tab; scanning rendered pages means no inline PR annotations.

**junit**: one test case per (test × scan point), violations as failure text — for CI dashboards
that speak nothing else.

**GitHub extras** (auto-enabled when `GITHUB_ACTIONS=true`): a markdown table to
`$GITHUB_STEP_SUMMARY` (new violations, top rules, fixable-debt count); job outputs written via
`$GITHUB_OUTPUT`: `new-violations`, `baselined-violations`, `stale-baseline-entries`, `tests-run`,
`report-file`.

## 10. GitHub Action (frozen interface)

Composite action in this repo, released with the binary, tagged `v1`/`v1.x.y` with the `v1` major
tag moved on each release — users pin `@v1`, never `@main`, and there is no separate drifting
"version" input; the action version *is* the tool version. The action downloads the matching release
binary for the runner OS/arch (cached via tool-cache).

```yaml
- uses: aquia-inc/frostfall@v1
  with:
    config: .frostfall.yml        # default: empty (CLI finds .frostfall.yml, then frostfall.yml)
    serve: ./dist                 # optional: static mode override
    base-url: ""                  # optional: running-server mode override
    baseline: .frostfall-baseline.json
    format: sarif                 # default: html
    output: ""                    # default: the format's file (frostfall.sarif for sarif)
    profile: ""                   # empty = no flag (ci applies automatically); "none" = base config
    id: ""                        # --id regex
    path: ""                      # --path regex
    discover: ""                  # "true" = --discover
    screenshots: ""               # "" = CLI default (on in Actions); "true"/"false" = --screenshots/--no-screenshots
- uses: github/codeql-action/upload-sarif@v3
  if: always()
  with:
    sarif_file: frostfall.sarif
```

Inputs map 1:1 onto CLI flags; `scripts/run.sh` builds the argument list from `FF_*` env vars, never from
a script body. Unlike Emberfall, no inline-config-string input in v1: a11y configs
reference steps and auth and belong in a file; inline configs also created Emberfall's runs-twice
footgun when both were passed.

## 11. Repo layout and distribution

```
frostfall/
├── cmd/frostfall/            # main
├── internal/
│   ├── config/               # schema, validation, env interpolation
│   ├── server/               # running / spawn / static
│   ├── browser/              # rod session, readiness, steps
│   ├── engine/               # Engine interface
│   │   └── axe/              # embedded axe.min.js + adapter
│   ├── fingerprint/
│   ├── baseline/
│   └── format/               # text, json, sarif, junit, github
├── action.yml
├── testdata/                 # fixture apps: static, vite-spa, next-app (built in CI)
└── docs/
```

Distribution mirrors Emberfall: goreleaser → GitHub releases (linux/darwin × amd64/arm64, windows
amd64), `brew tap aquia-inc/frostfall`, `go install`, and the action. axe-core is vendored at a
pinned version and bumped by PR (each bump is user-visible via the baseline version warning, §5).

Integration tests run the real binary against the `testdata` fixture apps with known-planted
violations, asserting on JSON output — including the fingerprint-stability suite: a set of DOM
mutations (class hash churn, sibling insertion, id regeneration) that must NOT change fingerprints,
and real changes that must.

## 12. Future work (explicitly out of v1)

- Next.js routes-manifest parsing for free route enumeration
- `standard: lighthouse` preset pinning Lighthouse's curated axe rule subset, for teams migrating
  from Lighthouse-based 508 checks who want run-over-run parity with historical results (Frostfall's
  default coverage is a strict superset of Lighthouse's accessibility category)
- Additional engines (IBM Equal Access, Lighthouse-as-engine for perf-curious users)
- Strict result mode paralleling Emberfall's planned strict JSON matching
- Multi-viewport sweeps (mobile breakpoint scans from one test definition)
- Accessible name/role assertions for arbitrary selectors (expectFocus covers
  the focused element only)
