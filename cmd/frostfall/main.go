// Command frostfall audits the rendered DOM of web apps for accessibility
// violations. See DESIGN.md for the full contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/aquia-inc/frostfall/internal/appname"
	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/discover"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/engine/axe"
	"github.com/aquia-inc/frostfall/internal/format"
	"github.com/aquia-inc/frostfall/internal/github"
	"github.com/aquia-inc/frostfall/internal/runner"
	"github.com/aquia-inc/frostfall/internal/scaffold"
	"github.com/aquia-inc/frostfall/internal/server"
)

// Exit codes are a frozen contract (DESIGN.md §3): CI must be able to tell
// "your pages are broken" from "the tool couldn't run".
const (
	exitOK          = 0
	exitViolations  = 1
	exitBadConfig   = 2
	exitEnvironment = 3
)

// version is set by goreleaser ldflags.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "init" {
		return runInit()
	}
	fs := flag.NewFlagSet("frostfall", flag.ContinueOnError)
	var (
		configPath     = fs.String("config", "", "config file (default: .frostfall.yml, frostfall.yml)")
		baseURL        = fs.String("base-url", "", "attach mode: scan an already-running server")
		serveDir       = fs.String("serve", "", "static mode: serve this directory")
		baselinePath   = fs.String("baseline", "", "baseline file (overrides config)")
		updateBaseline = fs.Bool("update-baseline", false, "rewrite the baseline to match current results")
		idFilter       = fs.String("id", "", "filter tests by id (Go regex)")
		pathFilter     = fs.String("path", "", "filter tests by path (Go regex)")
		browserPath    = fs.String("browser-path", "", "Chrome/Chromium binary to use")
		screenshots    = fs.Bool("screenshots", false, "capture element screenshots of new violations")
		screenshotDir  = fs.String("screenshot-dir", "frostfall-artifacts", "directory for violation screenshots")
		verbose        = fs.Bool("verbose", false, "per-step logging")
		showVersion    = fs.Bool("version", false, "print version and exit")
		validateOnly   = fs.Bool("validate", false, "validate the config and exit")
		profile        = fs.String("profile", "", "config profile to apply (default: ci when running in CI and a ci profile exists; 'none' to force the base config)")
		ghIssues       = fs.Bool("gh-issues", false, "file/maintain GitHub issues for failing violations (needs GITHUB_TOKEN and GITHUB_REPOSITORY)")
		ghIssuesDry    = fs.Bool("gh-issues-dry-run", false, "print the issue actions a --gh-issues run would take, without calling GitHub")
	)
	formatName := fs.String("format", "text", "output format: text|html|sarif (json, junit planned)")
	outputPath := fs.String("output", "", "write the formatted report here (default for html: frostfall-report.html)")
	fs.Bool("watch", false, "rescan on change, print only new violations")
	discoverFlag := fs.Bool("discover", false, "crawl same-origin paths from the root and scan discovered pages")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitBadConfig
	}
	// Fail fast on flag mistakes: a bad --format must not burn a full scan
	// before erroring, and a filtered --update-baseline would silently wipe
	// baseline entries for every test the filter excluded.
	if *formatName != "text" && *formatName != "html" && *formatName != "sarif" {
		fmt.Fprintf(os.Stderr, "unknown format %q (available: text, html, sarif)\n", *formatName)
		return exitBadConfig
	}
	if fs.Lookup("watch").Value.String() == "true" {
		fmt.Fprintln(os.Stderr, "--watch is not implemented yet")
		return exitBadConfig
	}
	if *updateBaseline && (*idFilter != "" || *pathFilter != "") {
		fmt.Fprintln(os.Stderr, "--update-baseline cannot be combined with --id/--path: a filtered update would drop baseline entries for the excluded tests")
		return exitBadConfig
	}

	if *showVersion {
		fmt.Printf("frostfall %s (axe-core %s)\n", version, axe.New().Version())
		return exitOK
	}

	// No explicit --profile means auto: the "ci" profile applies when running
	// under CI and defined; --profile none forces the base config.
	profileReq := *profile
	if profileReq == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		profileReq = config.ProfileAuto
	}
	cfg, err := loadConfig(*configPath, profileReq)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitBadConfig
	}
	if *validateOnly {
		fmt.Println("config OK")
		return exitOK
	}

	tests, err := filterTests(cfg.Tests, *idFilter, *pathFilter)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitBadConfig
	}
	if len(tests) == 0 && !*discoverFlag {
		fmt.Fprintln(os.Stderr, "no tests match the given filters")
		return exitBadConfig
	}

	// Resolve the base URL from mode overrides or the config's server block.
	base, cleanup, code := resolveServer(cfg, *baseURL, *serveDir)
	if code != exitOK {
		return code
	}
	defer cleanup()

	bp := cfg.Baseline
	if *baselinePath != "" {
		bp = *baselinePath
	}
	// Same default on the read path as --update-baseline uses on the write
	// path: a config without baseline: must still read back the file that a
	// bare --update-baseline run wrote, or the baseline silently never
	// applies. The default pickup is announced (a stale file must not
	// suppress violations invisibly), and --baseline none opts out.
	defaulted := false
	if bp == "" {
		bp, defaulted = ".frostfall-baseline.json", true
	}
	if bp == "none" {
		bp = ""
	}
	var bl *baseline.File
	if bp != "" {
		if bl, err = baseline.Load(bp); err != nil {
			fmt.Fprintln(os.Stderr, "baseline error:", err)
			return exitBadConfig
		}
		if defaulted && len(bl.Violations) > 0 {
			fmt.Fprintf(os.Stderr, "using baseline %s (default path; pass --baseline none to ignore it)\n", bp)
		}
	}

	eng := axe.New()
	if bl != nil && bl.AxeVersion != "" && bl.AxeVersion != eng.Version() {
		fmt.Fprintf(os.Stderr,
			"warning: baseline was created with axe-core %s but this binary embeds %s; run --update-baseline to realign\n",
			bl.AxeVersion, eng.Version())
	}

	b, err := browser.Launch(*browserPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser error:", err)
		return exitEnvironment
	}
	defer b.Close()

	// Screenshots default on in CI (evidence artifacts), off locally (speed);
	// the flag forces them on either way.
	shotDir := ""
	if *screenshots || os.Getenv("GITHUB_ACTIONS") == "true" {
		shotDir = *screenshotDir
	}
	r := &runner.Runner{
		Browser:       b,
		Engine:        eng,
		Config:        cfg,
		BaseURL:       base,
		Baseline:      bl,
		ScreenshotDir: shotDir,
		Verbose:       *verbose,
		Log:           func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	if *discoverFlag {
		discovered, derr := discoverTests(b, base, cfg, tests)
		if derr != nil {
			fmt.Fprintln(os.Stderr, "discover error:", derr)
			return exitEnvironment
		}
		fmt.Fprintf(os.Stderr, "discovered %d page(s) beyond the configured tests\n", len(discovered))
		tests = append(tests, discovered...)
	}

	res, err := r.Execute(context.Background(), tests)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run error:", err)
		return exitEnvironment
	}

	if *updateBaseline {
		if bp == "" {
			bp = ".frostfall-baseline.json"
		}
		f := r.ToBaseline(res)
		f.Created = time.Now().UTC()
		if err := f.Save(bp); err != nil {
			fmt.Fprintln(os.Stderr, "baseline error:", err)
			return exitEnvironment
		}
		fmt.Printf("baseline written to %s (%d violations accepted)\n", bp, len(f.Violations))
		return exitOK
	}

	exp := cfg.Defaults.Expect
	perTest := map[string]config.Expect{}
	enforcing := exp.Enforcing()
	for _, t := range tests {
		if t.Expect != nil {
			perTest[t.ID] = *t.Expect
			enforcing = enforcing || t.Expect.Enforcing()
		}
	}
	// The report marks serious+ violations even when the default expect is
	// report-only; an explicit default contract replaces that floor. Keyed on
	// the DEFAULT expect (not per-test enforcement) so tests without an
	// override keep their informational serious+ markers.
	reportDef := exp
	if !exp.Enforcing() {
		reportDef = config.Expect{Severity: engine.Serious.String()}
	}
	flagged := func(v runner.Result) bool { return runner.Failing(v, reportDef, perTest) }

	// The summary label only claims contract breakage when the flagged set is
	// exactly the contract set (the default expect enforces). With per-test
	// contracts layered on a report-only default, the count includes
	// informational serious+ rows, so the label must not overclaim.
	label := "flagged (serious or worse)"
	if exp.Enforcing() {
		label = "breaking the expect contract"
		if exp.MaxViolations != nil && *exp.MaxViolations > 0 {
			label += fmt.Sprintf(" (up to %d tolerated)", *exp.MaxViolations)
		}
	}
	format.Text(os.Stdout, res, flagged, label, enforcing)

	switch *formatName {
	case "text": // already written above
	case "sarif":
		out := *outputPath
		if out == "" {
			out = "frostfall.sarif"
		}
		f, ferr := os.Create(out)
		if ferr == nil {
			ferr = format.SARIF(f, res, format.RunMeta{
				ToolVersion: version,
				AxeVersion:  eng.Version(),
				Standard:    cfg.Defaults.Standard,
				Mode:        modeLabel(exp, enforcing),
			}, flagged)
			f.Close()
		}
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "report error:", ferr)
			return exitEnvironment
		}
		fmt.Printf("report written to %s\n", out)
	case "html":
		out := *outputPath
		if out == "" {
			out = "frostfall-report.html"
		}
		f, ferr := os.Create(out)
		if ferr == nil {
			ferr = format.HTML(f, res, format.RunMeta{
				Date:        time.Now(),
				App:         appname.Detect(".", cfg.Name),
				BaseURL:     base,
				Standard:    cfg.Defaults.Standard,
				Profile:     activeProfileName(profileReq, cfg),
				ToolVersion: version,
				AxeVersion:  eng.Version(),
				Mode:        modeLabel(exp, enforcing),
			}, flagged)
			f.Close()
		}
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "report error:", ferr)
			return exitEnvironment
		}
		fmt.Printf("report written to %s\n", out)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %q (available: text, html)\n", *formatName)
		return exitBadConfig
	}

	writeGitHubOutputs(res, flagged, *formatName, *outputPath)

	if *ghIssues || *ghIssuesDry {
		if err := syncIssues(res, flagged, *ghIssuesDry); err != nil {
			// Issue filing must never break the scan; report and move on.
			fmt.Fprintln(os.Stderr, "gh-issues warning:", err)
		}
	}

	if enforcing {
		limit := 0
		if exp.MaxViolations != nil {
			limit = *exp.MaxViolations
		}
		if res.EnforcedFailures(exp, perTest) > limit {
			return exitViolations
		}
	}
	return exitOK
}

// writeGitHubOutputs exposes run counts as job outputs when running in
// Actions, so downstream steps can gate without parsing the report.
func writeGitHubOutputs(res *runner.Run, flagged func(runner.Result) bool, formatName, outputPath string) {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	baselined, flaggedCount := 0, 0
	for _, r := range res.Results {
		if r.Baselined {
			baselined++
		} else if flagged(r) {
			flaggedCount++
		}
	}
	fmt.Fprintf(f, "new-violations=%d\n", flaggedCount)
	fmt.Fprintf(f, "baselined-violations=%d\n", baselined)
	fmt.Fprintf(f, "stale-baseline-entries=%d\n", len(res.Stale))
	fmt.Fprintf(f, "tests-run=%d\n", res.TestsRun)
	if formatName == "html" || formatName == "sarif" {
		if outputPath == "" {
			outputPath = map[string]string{"html": "frostfall-report.html", "sarif": "frostfall.sarif"}[formatName]
		}
		fmt.Fprintf(f, "report-file=%s\n", outputPath)
	}
}

// syncIssues reconciles GitHub issues against this run's failing violations —
// the shared enforcement predicate, so filed issues always match what the
// exit code and reports flag. Close scoping uses the tests that actually
// executed a scan — a configured test whose scan never ran must not close its
// issues as "fixed".
func syncIssues(res *runner.Run, flagged func(runner.Result) bool, dryRun bool) error {
	var failing []runner.Result
	for _, r := range res.Results {
		if flagged(r) {
			failing = append(failing, r)
		}
	}
	groups := github.BuildGroups(failing)
	testsRun := res.Scanned

	if dryRun {
		// Without credentials we cannot see existing issues, so a dry run
		// plans against an empty set: it shows the full desired state.
		for _, a := range github.Plan(groups, nil, testsRun) {
			fmt.Println("gh-issues (dry-run):", a)
		}
		return nil
	}

	token, repo := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_REPOSITORY")
	if token == "" || repo == "" {
		return fmt.Errorf("GITHUB_TOKEN and GITHUB_REPOSITORY must be set")
	}
	client := github.NewClient(token, repo)
	if err := client.EnsureLabel(github.Label, "1f5eff", "Filed by the Frostfall accessibility scanner"); err != nil {
		return err
	}
	existing, err := client.ListLabeled(github.Label)
	if err != nil {
		return err
	}
	runContext := "Filed by Frostfall."
	if server, run := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_RUN_ID"); server != "" && run != "" {
		runContext = fmt.Sprintf("Filed by Frostfall ([run](%s/%s/actions/runs/%s)).", server, repo, run)
	}
	actions, err := github.Sync(client, groups, existing, testsRun, runContext)
	for _, a := range actions {
		fmt.Println("gh-issues:", a)
	}
	return err
}

// modeLabel describes the enforcement posture for report headers, derived
// from the same inputs as the flagged predicate so it cannot mislabel a
// rules-only or per-test contract as a severity floor.
func modeLabel(def config.Expect, enforcing bool) string {
	if !enforcing {
		return "report only"
	}
	switch {
	case def.Severity != "" && len(def.Rules) > 0:
		return fmt.Sprintf("enforcing (%s+ and %d rule(s))", def.Severity, len(def.Rules))
	case def.Severity != "":
		return "enforcing (" + def.Severity + "+)"
	case len(def.Rules) > 0:
		return "enforcing (rules)"
	default:
		return "enforcing (per-test contracts)"
	}
}

// activeProfileName resolves what profile actually applied, for report
// headers.
func activeProfileName(req string, cfg *config.Config) string {
	switch req {
	case "", config.ProfileNone:
		return ""
	case config.ProfileAuto:
		if _, ok := cfg.Profiles["ci"]; ok {
			return "ci"
		}
		return ""
	default:
		return req
	}
}

// discoverTests crawls from the root and synthesizes fresh-load tests for
// pages no explicit test covers. Explicit tests always win on a path
// collision — discovery adds breadth, never overrides depth.
func discoverTests(b *browser.Browser, base string, cfg *config.Config, explicit []config.Test) ([]config.Test, error) {
	opts := config.Discover{MaxDepth: 3, MaxPages: 100}
	if cfg.Discover != nil {
		opts = *cfg.Discover
	}
	crawler := &discover.Crawler{
		Browser:  b,
		BaseURL:  base,
		Options:  opts,
		Defaults: cfg.Defaults,
		Log:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	paths, err := crawler.Run()
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, t := range explicit {
		covered[t.Path] = true
	}
	var out []config.Test
	for _, p := range paths {
		if !covered[p] {
			out = append(out, config.Test{ID: "discovered:" + p, Path: p})
		}
	}
	return out, nil
}

// runInit detects the project in the working directory and writes a tailored
// starter config. Never overwrites an existing one.
func runInit() int {
	for _, existing := range []string{".frostfall.yml", "frostfall.yml"} {
		if _, err := os.Stat(existing); err == nil {
			fmt.Fprintf(os.Stderr, "%s already exists; delete it first to re-init\n", existing)
			return exitBadConfig
		}
	}
	p := scaffold.Detect(".")
	if err := os.WriteFile(".frostfall.yml", []byte(scaffold.Render(p)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		return exitEnvironment
	}
	fmt.Printf("detected %s project — wrote .frostfall.yml\n", p.Framework)
	fmt.Println("next steps:")
	fmt.Println("  1. review .frostfall.yml and add your routes")
	fmt.Println("  2. frostfall --update-baseline   # accept existing violations")
	fmt.Println("  3. frostfall                     # fails only on new violations")
	return exitOK
}

// resolveServer picks the server mode and returns the base URL plus teardown.
func resolveServer(cfg *config.Config, baseURLFlag, serveFlag string) (string, func(), int) {
	noop := func() {}
	switch {
	case baseURLFlag != "":
		return probeAttach(baseURLFlag, noop)
	case serveFlag != "":
		return startStatic(serveFlag, true)
	case cfg.Server == nil:
		fmt.Fprintln(os.Stderr, "no server configured: set server: in config or pass --base-url / --serve")
		return "", noop, exitBadConfig
	case cfg.Server.BaseURL != "":
		return probeAttach(cfg.Server.BaseURL, noop)
	case cfg.Server.Serve != "":
		spa := cfg.Server.SPAFallback == nil || *cfg.Server.SPAFallback
		return startStatic(cfg.Server.Serve, spa)
	default:
		fmt.Fprintln(os.Stderr, "server.command (spawn mode) is not implemented yet; use --base-url or --serve")
		return "", noop, exitEnvironment
	}
}

func probeAttach(url string, cleanup func()) (string, func(), int) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || resp.StatusCode >= 500 {
		fmt.Fprintf(os.Stderr,
			"cannot reach %s — is the server running? (or use --serve for a static build)\n", url)
		return "", cleanup, exitEnvironment
	}
	return url, cleanup, exitOK
}

func startStatic(dir string, spa bool) (string, func(), int) {
	srv := &server.Static{Dir: dir, SPAFallback: spa}
	url, err := srv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve error:", err)
		return "", func() {}, exitEnvironment
	}
	return url, func() { srv.Stop() }, exitOK
}

func filterTests(tests []config.Test, idPat, pathPat string) ([]config.Test, error) {
	if idPat == "" && pathPat == "" {
		return tests, nil
	}
	idRe, err := regexp.Compile(idPat)
	if err != nil {
		return nil, fmt.Errorf("--id: %w", err)
	}
	pathRe, err := regexp.Compile(pathPat)
	if err != nil {
		return nil, fmt.Errorf("--path: %w", err)
	}
	var out []config.Test
	for _, t := range tests {
		if idRe.MatchString(t.ID) && pathRe.MatchString(t.Path) {
			out = append(out, t)
		}
	}
	return out, nil
}

func loadConfig(path, profile string) (*config.Config, error) {
	if path != "" {
		return config.Load(path, profile)
	}
	for _, candidate := range []string{".frostfall.yml", "frostfall.yml"} {
		if _, err := os.Stat(candidate); err == nil {
			return config.Load(candidate, profile)
		}
	}
	return nil, fmt.Errorf("no config found (.frostfall.yml or frostfall.yml); use --config")
}
