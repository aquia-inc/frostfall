# Frostfall — Design Document

Accessibility test suite for rendered web apps. Sibling to [Emberfall](https://github.com/aquia-inc/emberfall):
Emberfall smoke-tests HTTP APIs with declarative YAML; Frostfall audits the rendered DOM of real
frontends (Vite, Next.js, CRA, anything that ships to a browser) for WCAG violations, locally and in CI.

Status: MVP implemented. The sections marked **(frozen)** are the expensive-to-change
contracts — config schema, violation fingerprinting, engine interface, exit codes, action interface.
Everything else is guidance the implementer may adapt.

Not yet implemented (config for these is rejected or flagged, never silently ignored):
server spawn mode (`server.command`), `auth:`, `--watch`, `--parallel`, and the
json/sarif/junit formatters.

---

## 1. Goals and non-goals

Goals:

- Single static Go binary. No Node, no npm install, no Docker required to run.
- Declarative YAML config in the Emberfall style: a list of tests, an `expect` block, sane defaults.
- Scans real rendered DOM, including interaction states (modals, dropdowns, post-validation forms),
  not just initial page loads.
- Works in three server modes without the user thinking about it: attach to a running dev server,
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
│                    (attach|spawn|serve)                │
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
  --base-url URL       attach mode: scan an already-running server
  --serve DIR          static mode: serve DIR with the embedded file server
  --baseline PATH      baseline file (default: value from config, else none)
  --update-baseline    rewrite the baseline to match current results, exit 0
  --format F           text|json|sarif|junit (repeatable: --format sarif --format text)
  --output PATH        write the (first non-text) format here; text always goes to stdout
  --id REGEX           filter tests by id (Go regex, like Emberfall's --url/--method)
  --path REGEX         filter tests by path
  --watch              dev loop: rescan on change, print only newly introduced violations
  --discover           crawl same-origin from the root path, respecting discover config (§7.6)
  --browser-path PATH  use this Chrome/Chromium instead of auto-detection
  --verbose            per-step logging
  --version / --help
```

Precedence: flags override config. `--base-url` and `--serve` each override the config's `server`
block entirely (attach and static mode respectively).

### Exit codes (frozen)

| code | meaning |
|------|---------|
| 0 | ran to completion, no enforced failures (report-only mode always exits 0; violations may exist but are reported, baselined, or below the enforcement contract) |
| 1 | new violations breaking a configured `expect` contract |
| 2 | config invalid (schema error, unknown key, bad regex) |
| 3 | environment failure (browser launch, server never became ready, page load timeout, step failure) |

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
  # Mode 1 — attach, mutually exclusive with both:
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
      - waitFor: "[data-testid=app-shell]"
  reuse: true                  # persist cookies+storage once, restore per test (default true)
  # storageState: .auth/state.json   # alternative: inject pre-captured state, skip setup

baseline: .frostfall-baseline.json

discover:                      # only used with --discover; see §7.6
  maxDepth: 3
  exclude: ["/admin/.*", "/logout"]
  maxPages: 100

tests:
  - id: home                   # required, unique; used in filtering, baseline entries, output
    path: /                    # relative to base URL; absolute url: also accepted for cross-origin
    scan: true                 # scan the initial state after readiness (default true)

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

- **Attach** (`baseUrl` / `--base-url`): probe once for a non-5xx response before starting; if
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

### 7.5 Auth

`setup` runs once against the first browser context; on success, cookies + localStorage +
sessionStorage are captured and injected into every subsequent context (`reuse: true`). Alternative:
`storageState` points at a Playwright-compatible storage-state JSON captured elsewhere, skipping the
flow entirely — the CI-friendly path when credentials shouldn't be near this config. Secrets only via
`${ENV}` interpolation; a literal-looking password in `auth` is a lint warning.

### 7.6 Discovery

`--discover` crawls same-origin `href`s from `/` (breadth-first, `maxDepth`, `exclude` regexes,
`maxPages`), deduplicates by path, and synthesizes fresh-load tests with generated ids
(`discovered:/pricing`). Two SPA realities learned from dogfooding: hash-router routes (`/#/users`)
keep the fragment as path identity rather than collapsing into `/`, and numeric path segments
dedupe by shape (`/items/1` and `/items/999` are one route, one representative scan). Discovered tests use `defaults` for everything. Explicit tests always win on
path collision. Discovery is additive to the config, not a replacement — the recommended shape is a
handful of explicit stateful tests plus discovery for coverage breadth. (Next.js routes-manifest
parsing is future work, §10.)

### 7.7 Watch mode

`--watch` implies attach mode. Initial full run establishes the session's "seen" set (baseline +
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
```

Fingerprinting, baselining, and formatting all consume `[]Violation` and know nothing about axe.
That is the whole point: pa11y-style HTML_CodeSniffer, IBM Equal Access, or a Lighthouse-as-engine
adapter slot in behind this without touching anything downstream. v1 registers only `axe`.

## 9. Output

**text** (default, stdout): grouped by test → scan point, colorized, impact-sorted, baselined
violations collapsed to a count, summary block last (new / baselined / stale-baselined / tests run).

**json**: complete structured results — every violation with fingerprint, baselined flag, raw target,
snippet, help URL, plus run metadata (frostfall version, axe version, base URL, timing).

**sarif** (2.1.0): one rule per axe rule with `helpUri`; results carry the page URL as the artifact
location and the DOM selector in `logicalLocations`; baselined violations map to
`"baselineState": "unchanged"`, new ones `"new"`. This is the flagship CI format — uploaded to GitHub
code scanning it renders violations as PR annotations, which almost nothing in the a11y space does.

**junit**: one test case per (test × scan point), violations as failure text — for CI dashboards
that speak nothing else.

**GitHub extras** (auto-enabled when `GITHUB_ACTIONS=true`): a markdown table to
`$GITHUB_STEP_SUMMARY` (new violations, top rules, fixable-debt count); job outputs written via
`$GITHUB_OUTPUT`: `new-violations`, `baselined-violations`, `stale-baseline-entries`, `tests-run`,
`sarif-file`.

## 10. GitHub Action (frozen interface)

Composite action in this repo, released with the binary, tagged `v1`/`v1.x.y` with the `v1` major
tag moved on each release — users pin `@v1`, never `@main`, and there is no separate drifting
"version" input; the action version *is* the tool version. The action downloads the matching release
binary for the runner OS/arch (cached via tool-cache).

```yaml
- uses: aquia-inc/frostfall@v1
  with:
    config: .frostfall.yml        # default
    serve: ./dist                 # optional: static mode override
    base-url: ""                  # optional: attach mode override
    baseline: .frostfall-baseline.json
    format: sarif                 # default in CI
    output: frostfall.sarif
- uses: github/codeql-action/upload-sarif@v3
  if: always()
  with:
    sarif_file: frostfall.sarif
```

Inputs map 1:1 onto CLI flags. Unlike Emberfall, no inline-config-string input in v1: a11y configs
reference steps and auth and belong in a file; inline configs also created Emberfall's runs-twice
footgun when both were passed.

## 11. Repo layout and distribution

```
frostfall/
├── cmd/frostfall/            # main
├── internal/
│   ├── config/               # schema, validation, env interpolation
│   ├── server/               # attach / spawn / static
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
- Screen-reader-tree assertions (accessible name/role expectations per selector)
