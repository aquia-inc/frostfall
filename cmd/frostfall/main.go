// Command frostfall audits the rendered DOM of web apps for accessibility
// violations. See DESIGN.md for the full contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

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
	"github.com/aquia-inc/frostfall/internal/secrets"
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

// rodArgRe is how go-rod spots its own options in os.Args, in a package init
// that runs before main: -rod=... or --rod=..., or -rod/--rod followed by
// the options as the next argument.
var rodArgRe = regexp.MustCompile(`^--?rod(=|$)`)

// interrupted is set by the attach-mode signal handler before it closes the
// browser. From then on the handler's exit is the run's outcome: the main
// goroutine, whose browser calls now fail, must not print an error or write a
// report of its own (haltIfInterrupted).
var interrupted atomic.Bool

// haltIfInterrupted parks the calling goroutine for good once a signal has
// been handled, so the handler's os.Exit decides how the run ends.
func haltIfInterrupted() {
	if interrupted.Load() {
		select {}
	}
}

func run(args []string) int {
	// go-rod applies -rod options found anywhere in os.Args (cdp logging, a
	// control URL, a visible browser) before main runs. cdp logging prints
	// every CDP message, which in attach mode includes every cookie of the
	// user's profile, HttpOnly ones too. No such argument reaches a browser:
	// refuse it, even as the value of another flag.
	for _, a := range args {
		if rodArgRe.MatchString(a) {
			fmt.Fprintln(os.Stderr, "refusing an argument that starts with -rod: go-rod reads it as its own debug options (its CDP log would print the browser's cookies), so it is not accepted anywhere on the command line")
			return exitBadConfig
		}
	}
	if len(args) > 0 && args[0] == "init" {
		return runInit()
	}
	fs := flag.NewFlagSet("frostfall", flag.ContinueOnError)
	var (
		configPath     = fs.String("config", "", "config file (default: .frostfall.yml, frostfall.yml)")
		baseURL        = fs.String("base-url", "", "running-server mode: scan an already-running server")
		serveDir       = fs.String("serve", "", "static mode: serve this directory")
		baselinePath   = fs.String("baseline", "", "baseline file (overrides config)")
		updateBaseline = fs.Bool("update-baseline", false, "rewrite the baseline to match current results")
		idFilter       = fs.String("id", "", "filter tests by id (Go regex)")
		pathFilter     = fs.String("path", "", "filter tests by path (Go regex)")
		browserPath    = fs.String("browser-path", "", "Chrome/Chromium binary to use")
		browserURL     = fs.String("browser-url", "", "scan with the session of a Chrome you started with --remote-debugging-port and signed in to, at this DevTools address (http://localhost:9222 or a ws:// browser URL, this machine only); needs auth.attach: true")
		screenshots    = fs.Bool("screenshots", false, "capture element screenshots of new violations")
		noScreenshots  = fs.Bool("no-screenshots", false, "disable screenshots even where they default on (CI)")
		screenshotDir  = fs.String("screenshot-dir", "frostfall-artifacts", "directory for violation screenshots")
		verbose        = fs.Bool("verbose", false, "per-step logging")
		showVersion    = fs.Bool("version", false, "print version and exit")
		validateOnly   = fs.Bool("validate", false, "validate the config and exit")
		profile        = fs.String("profile", "", "config profile to apply (default: ci when running in CI and a ci profile exists; 'none' to force the base config)")
		ghIssues       = fs.Bool("gh-issues", false, "file/maintain GitHub issues for failing violations (needs GITHUB_TOKEN and GITHUB_REPOSITORY)")
		ghIssuesDry    = fs.Bool("gh-issues-dry-run", false, "print the issue actions a --gh-issues run would take, without calling GitHub")
	)
	formatName := fs.String("format", "text", "output format: text|html|sarif|json|junit (text is always printed; the others are also written to --output)")
	outputPath := fs.String("output", "", "write the formatted report here (default: frostfall-report.html, frostfall.sarif, frostfall.json or frostfall-junit.xml by format)")
	fs.Bool("watch", false, "rescan on change, print only new violations")
	discoverFlag := fs.Bool("discover", false, "crawl same-origin paths from the root and scan discovered pages")
	dryRun := fs.Bool("dry-run", false, "with --discover: print the discovered paths, one per line, and scan nothing")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitBadConfig
	}
	// Whether a flag was passed at all, asked of the flag set: --browser-url=
	// is set but empty, which must not read as "not attached".
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	// Fail fast on flag mistakes: a bad --format must not burn a full scan
	// before erroring, and a filtered --update-baseline would silently wipe
	// baseline entries for every test the filter excluded.
	if _, known := reportFiles[*formatName]; !known && *formatName != "text" {
		fmt.Fprintf(os.Stderr, "unknown format %q (available: text, html, sarif, json, junit)\n", *formatName)
		return exitBadConfig
	}
	if *dryRun {
		// A dry run only lists what the crawl finds: it has no scan to dry-run
		// on its own, and nothing it could write. --format has a default, so
		// whether it was passed is asked of the flag set, not read off its value.
		set := given
		switch {
		case !*discoverFlag:
			fmt.Fprintln(os.Stderr, "--dry-run only applies to discovery: pass --discover --dry-run to list the pages a crawl finds")
			return exitBadConfig
		case *updateBaseline:
			fmt.Fprintln(os.Stderr, "--dry-run scans nothing, so there is no baseline to update: drop --update-baseline")
			return exitBadConfig
		case *ghIssues || *ghIssuesDry:
			var given []string
			if *ghIssues {
				given = append(given, "--gh-issues")
			}
			if *ghIssuesDry {
				given = append(given, "--gh-issues-dry-run")
			}
			fmt.Fprintln(os.Stderr, "--dry-run scans nothing, so there are no issues to file: drop "+strings.Join(given, " and "))
			return exitBadConfig
		case *validateOnly:
			fmt.Fprintln(os.Stderr, "--dry-run lists the pages a crawl finds and does not validate the config: drop --validate")
			return exitBadConfig
		case set["format"] || set["output"]:
			fmt.Fprintln(os.Stderr, "--dry-run prints a path list, not a report, so --format and --output have no effect: drop them")
			return exitBadConfig
		}
	}

	started := time.Now()
	// The dry-run checks above return before abort exists, so a refused dry
	// run writes nothing. An accepted one cannot name json or junit either;
	// !*dryRun says outright that a dry run never writes a report.
	structured := (*formatName == "json" || *formatName == "junit") && !*dryRun
	// What is known so far, for a failure report: config and tests are filled
	// in as the run gets that far.
	var (
		cfg        *config.Config
		tests      []config.Test
		base       string
		profileReq string
	)
	// abort stops the run before anything was scanned: it prints message the
	// way every failure does and, for json and junit, writes a failure report
	// over the output path. Otherwise an older report from a passing run would
	// still sit there and read as this run's result. text, html and sarif
	// leave their file alone, as they always have, and so does --validate,
	// which never writes a report.
	abort := func(code int, message string) int {
		haltIfInterrupted()
		fmt.Fprintln(os.Stderr, message)
		if structured && !*validateOnly {
			reportFailure(*formatName, *outputPath, cfg, tests, base, profileReq, started, code, &format.RunFailure{Message: message})
		}
		return code
	}
	// From here the format and output path are known, so these flag mistakes
	// go through abort: a json or junit report must not keep an older result.
	if fs.Lookup("watch").Value.String() == "true" {
		return abort(exitBadConfig, "--watch is not implemented yet")
	}
	if *updateBaseline && (*idFilter != "" || *pathFilter != "") {
		return abort(exitBadConfig, "--update-baseline cannot be combined with --id/--path: a filtered update would drop baseline entries for the excluded tests")
	}

	if *showVersion {
		fmt.Printf("frostfall %s (axe-core %s)\n", version, axe.New().Version())
		return exitOK
	}
	// The endpoint is checked before the config is read: a remote host or a
	// malformed URL is wrong whatever the config says. Errors show it as
	// scheme://host:port only, since a ws:// path is the browser's control key.
	var endpoint *browser.Endpoint
	if given["browser-url"] {
		if *browserURL == "" {
			return abort(exitBadConfig, "--browser-url: empty; give the DevTools address of the Chrome to attach to, such as http://localhost:9222")
		}
		if *browserPath != "" {
			return abort(exitBadConfig, "--browser-url attaches to a Chrome that is already running and --browser-path launches one: pass one or the other")
		}
		ep, perr := browser.ParseBrowserURL(*browserURL)
		if perr != nil {
			return abort(exitBadConfig, perr.Error())
		}
		endpoint = ep
	}

	// No explicit --profile means auto: the "ci" profile applies when running
	// under CI and defined; --profile none forces the base config.
	profileReq = *profile
	if profileReq == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		profileReq = config.ProfileAuto
	}
	cfg, err := loadConfig(*configPath, profileReq)
	if err != nil {
		return abort(exitBadConfig, "config error: "+err.Error())
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if msg := attachMismatch(cfg, endpoint != nil, *validateOnly, profileReq); msg != "" {
		return abort(exitBadConfig, "config error: "+msg)
	}
	if *validateOnly {
		// The filters are compiled and applied the way a real run would, so a bad
		// --id or --path regex fails here too, with the same error, whatever the
		// config has in it.
		selected, ferr := filterTests(cfg.Tests, *idFilter, *pathFilter)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "config error:", ferr)
			return exitBadConfig
		}
		// With auth, a test on another origin is a config error. Where the base
		// URL is known without starting anything (baseUrl or --base-url) that is
		// checked here; with serve: the origin is an ephemeral port, so it is
		// checked at scan time.
		if base := knownBaseURL(cfg, *baseURL, *serveDir); cfg.Auth != nil && base != "" {
			if cerr := cfg.CheckTestOrigins(base, selected); cerr != nil {
				fmt.Fprintln(os.Stderr, "config error:", cerr)
				return exitBadConfig
			}
		}
		fmt.Println("config OK")
		return exitOK
	}

	tests, err = filterTests(cfg.Tests, *idFilter, *pathFilter)
	if err != nil {
		return abort(exitBadConfig, "config error: "+err.Error())
	}
	if len(tests) == 0 && !*discoverFlag {
		return abort(exitBadConfig, "no tests match the given filters")
	}

	// Resolve the base URL from mode overrides or the config's server block.
	resolved, cleanup, code, serverErr := resolveServer(cfg, *baseURL, *serveDir)
	if code != exitOK {
		return abort(code, serverErr)
	}
	base = resolved
	defer cleanup()
	// The session reaches every host a test loads, so with auth a test must
	// stay on the app's origin (or opt out). Known only now: --base-url and
	// serve: decide the origin.
	// Only the tests this run selects: --id and --path must not be refused over
	// tests they do not run.
	if err := cfg.CheckTestOrigins(base, tests); err != nil {
		return abort(exitBadConfig, "config error: "+err.Error())
	}

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
	// A dry run scans nothing, so a baseline file, however broken, is not read.
	if bp != "" && !*dryRun {
		if bl, err = baseline.Load(bp); err != nil {
			return abort(exitBadConfig, "baseline error: "+err.Error())
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

	var b *browser.Browser
	stopSignals := func() {}
	if endpoint != nil {
		b, err = browser.Connect(endpoint)
	} else {
		b, err = browser.Launch(*browserPath)
	}
	if err != nil {
		return abort(exitEnvironment, "browser error: "+err.Error())
	}
	// Close never quits an attached browser: it removes what Frostfall opened
	// in it and disconnects. Every return from here on passes through it.
	defer b.Close()
	if b.Attached() {
		if *verbose {
			product := b.Version()
			if product == "" {
				product = "Chrome"
			}
			fmt.Fprintf(os.Stderr, "attached to %s at %s\n", product, b.Endpoint())
		}
		// A Ctrl-C skips the deferred Close. The contexts are created to be
		// disposed when the connection drops, but the tab the session is read
		// in is not, so an interrupt closes them all before exiting.
		stopSignals = closeOnSignal(b)
		defer stopSignals()
	}

	// Screenshots default on in CI (evidence artifacts), off locally (speed).
	// --screenshots forces on, --no-screenshots wins over everything: the CI
	// default previously had no opt-out at all.
	shotDir := ""
	if (*screenshots || os.Getenv("GITHUB_ACTIONS") == "true") && !*noScreenshots {
		shotDir = *screenshotDir
	}
	// Auth runs its login flow (reuse: true) here, once, before the first test
	// or the crawl: both need the session, and a broken login should stop the
	// run before any page is scanned signed out.
	authOpts := browser.AuthOptions{
		Warn: func(f string, a ...any) { fmt.Fprintf(os.Stderr, "warning: "+f+"\n", a...) },
		Note: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	if *verbose {
		authOpts.Log = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	}
	// Nothing to sign in for when every selected test says auth: false and
	// there is no discovery: skip the probe and the login flow, and keep only
	// the redaction a run with an auth block has.
	needAuth := *discoverFlag
	for _, t := range tests {
		if t.UsesAuth() {
			needAuth = true
			if authOpts.FirstTest == "" {
				authOpts.FirstTest = t.Path
				if t.URL != "" {
					authOpts.FirstTest = t.URL
				}
			}
		}
	}
	var authn *browser.Auth
	if cfg.Auth != nil && !needAuth {
		authn = browser.RedactingAuth()
	} else if authn, err = browser.NewAuth(b, cfg, base, authOpts); err != nil {
		return abort(exitEnvironment, "auth error: "+err.Error())
	}
	if *dryRun {
		if w := discoverAuthWarning(cfg); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		return runDryRun(b, authn, base, cfg)
	}
	r := &runner.Runner{
		Browser:       b,
		Auth:          authn,
		Engine:        eng,
		Config:        cfg,
		BaseURL:       base,
		Baseline:      bl,
		ScreenshotDir: shotDir,
		Verbose:       *verbose,
		Log:           func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		Warn:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, "warning: "+f+"\n", a...) },
	}
	if *discoverFlag {
		if w := discoverAuthWarning(cfg); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		discovered, derr := discoverTests(b, authn, base, cfg, tests)
		haltIfInterrupted()
		if derr != nil {
			return abort(exitEnvironment, "discover error: "+derr.Error())
		}
		fmt.Fprintf(os.Stderr, "discovered %d page(s) beyond the configured tests\n", len(discovered))
		tests = append(tests, discovered...)
	}

	// The contract is read from the final test list (discovered tests are
	// already in it). The run-error report below and the exit code both use
	// it; the aborts above, which happen before discovery finishes, work it
	// out from the tests known by then.
	exp, perTest, enforcing := expectContract(cfg, tests)

	res, err := r.Execute(context.Background(), tests)
	haltIfInterrupted()
	if b.Attached() {
		// Nothing after this needs the browser. Releasing the user's Chrome
		// now means an interrupt while the reports are written is an ordinary
		// one, as in launch mode, with nothing of Frostfall's left open. Close
		// first, then stop listening: a signal that came in meanwhile is
		// honoured by stopSignals (exit 128+n) instead of a report.
		b.Close()
		stopSignals()
	}
	if err != nil {
		runErr := "run error: " + err.Error()
		fmt.Fprintln(os.Stderr, runErr)
		// json and junit exist for CI to read, and an environment error is
		// something CI must be able to read there too. The run has no results,
		// only the error, in the same words stderr has just printed.
		if structured {
			failure := &format.RunFailure{Message: runErr}
			var terr *runner.TestError
			if errors.As(err, &terr) {
				failure.TestID = terr.TestID
			}
			reportFailure(*formatName, *outputPath, cfg, tests, base, profileReq, started, exitEnvironment, failure)
		}
		return exitEnvironment
	}

	if *updateBaseline {
		if bp == "" {
			bp = ".frostfall-baseline.json"
		}
		f := r.ToBaseline(res)
		f.Created = time.Now().UTC()
		if err := f.Save(bp); err != nil {
			return abort(exitEnvironment, "baseline error: "+err.Error())
		}
		fmt.Printf("baseline written to %s (%d violations accepted)\n", bp, len(f.Violations))
		return exitOK
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

	// Decided before the report is written so json and junit can carry it.
	// counted is what the gate counts: unlike flagged, it is empty under a
	// report-only contract. junit fails a testcase only when the gate fails,
	// so it reuses this decision instead of making its own.
	counted := func(v runner.Result) bool { return enforcing && runner.Failing(v, exp, perTest) }
	exitCode := exitOK
	limit := 0
	if enforcing {
		if exp.MaxViolations != nil {
			limit = *exp.MaxViolations
		}
		if res.EnforcedFailures(exp, perTest) > limit {
			exitCode = exitViolations
		}
	}

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
				BaseURL:     reportedBase(base, cfg),
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
	case "json", "junit":
		meta := structuredMeta(version, eng.Version(), reportedBase(base, cfg), cfg.Defaults.Standard,
			activeProfileName(profileReq, cfg), modeLabel(exp, enforcing), started, exitCode)
		meta.Enforcing = enforcing
		meta.GateFailed = exitCode == exitViolations
		meta.Counts = counted
		meta.Limit = limit
		meta.Redact = cfg.Auth != nil
		if ferr := writeStructured(*formatName, *outputPath, res, meta, flagged); ferr != nil {
			fmt.Fprintln(os.Stderr, "report error:", ferr)
			return exitEnvironment
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown format %q (available: text, html, sarif, json, junit)\n", *formatName)
		return exitBadConfig
	}

	writeGitHubOutputs(res, flagged, *formatName, *outputPath)

	if *ghIssues || *ghIssuesDry {
		if err := syncIssues(res, flagged, *ghIssuesDry); err != nil {
			// Issue filing must never break the scan; report and move on.
			fmt.Fprintln(os.Stderr, "gh-issues warning:", err)
		}
	}

	return exitCode
}

// attachMismatch pairs --browser-url with auth.attach: true, strictly, in the
// auth block in effect after the profile. The flag with any other block (or
// none) would scan signed out, or with some other session, in the user's own
// browser; the key without the flag has no browser to read a session from.
// --validate checks the config alone, so there the key needs no flag. ""
// when they agree.
func attachMismatch(cfg *config.Config, flag, validate bool, profileReq string) string {
	attach := cfg.Auth != nil && cfg.Auth.Attach
	where := "the config"
	if p := activeProfileName(profileReq, cfg); p != "" {
		where = fmt.Sprintf("the config with profile %q applied", p)
	}
	switch {
	case flag && cfg.Auth == nil:
		return "--browser-url takes the session from a running, signed-in Chrome, but " + where + " has no auth block: " +
			"add auth: { attach: true, signedIn: <a selector only signed-in pages render> }"
	case flag && !attach:
		return "--browser-url takes the session from a running, signed-in Chrome, but the auth block of " + where + " does not set attach: true: " +
			"set it in place of setup or storageState (a profile can switch auth sources), or drop --browser-url"
	case !flag && attach && !validate:
		return "auth.attach: true takes the session from a Chrome you started and signed in to: pass --browser-url with its DevTools address, " +
			"such as --browser-url http://localhost:9222 for Chrome started with --remote-debugging-port=9222"
	}
	return ""
}

// closeOnSignal closes b when the run is interrupted (SIGINT, SIGTERM,
// SIGHUP from a closed terminal, SIGQUIT), then exits the way a process
// killed by that signal is reported: 128 plus the signal number (130, 143,
// 129, 131). The returned function stops listening; see guardSignals.
func closeOnSignal(b *browser.Browser) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return guardSignals(b, sigs, func() { signal.Stop(sigs) }, os.Exit)
}

// guardSignals waits for a signal on sigs. One sets interrupted first, so the
// main goroutine, whose browser calls start failing, neither prints an error
// nor writes a report, then closes b and exits with signalExitCode. The
// returned function stops it: it unsubscribes, and a signal that arrived
// before that is still honoured, never dropped, so a run that ends just as
// the user presses Ctrl-C does not go on to write its report. It returns only
// when no signal is pending, and is safe to call more than once.
func guardSignals(b interface{ Close() }, sigs chan os.Signal, unsubscribe func(), exit func(int)) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	handle := func(sig os.Signal) {
		interrupted.Store(true)
		fmt.Fprintln(os.Stderr, "interrupted: closing what Frostfall opened in the attached browser (the browser itself stays open)")
		b.Close()
		exit(signalExitCode(sig))
	}
	go func() {
		defer close(finished)
		select {
		case sig := <-sigs:
			handle(sig)
		case <-done:
			// Stopped, but a signal may already be waiting: when both are ready
			// select picks either, so look once more.
			select {
			case sig := <-sigs:
				handle(sig)
			default:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			unsubscribe()
			close(done)
			<-finished
		})
	}
}

// signalExitCode is 128 plus the signal's number, as a shell reports a
// process that signal killed.
func signalExitCode(sig os.Signal) int {
	switch sig {
	case syscall.SIGHUP:
		return 129
	case syscall.SIGQUIT:
		return 131
	case syscall.SIGTERM:
		return 143
	}
	return 130
}

// expectContract reads the enforcement contract of a run: the default expect,
// the per-test overrides, and whether anything in them can fail the build.
func expectContract(cfg *config.Config, tests []config.Test) (exp config.Expect, perTest map[string]config.Expect, enforcing bool) {
	exp = cfg.Defaults.Expect
	perTest = map[string]config.Expect{}
	enforcing = exp.Enforcing()
	for _, t := range tests {
		if t.Expect != nil {
			perTest[t.ID] = *t.Expect
			enforcing = enforcing || t.Expect.Enforcing()
		}
	}
	return exp, perTest, enforcing
}

// reportFailure writes the json or junit report for a run that stopped before
// it produced results: the exit status, the error, and whatever of the run's
// context was known by then (cfg is nil when the config itself failed). The
// existing file is overwritten, never deleted, so a stale passing report can
// not outlive a failed run. A write error is reported and does not change the
// exit code the run is already failing with.
func reportFailure(formatName, out string, cfg *config.Config, tests []config.Test, base, profileReq string, started time.Time, code int, failure *format.RunFailure) {
	meta := structuredMeta(version, axe.New().Version(), "", "", "", "unknown", started, code)
	if cfg != nil {
		if tests == nil {
			tests = cfg.Tests
		}
		exp, _, enforcing := expectContract(cfg, tests)
		meta.BaseURL = reportedBase(base, cfg)
		meta.Standard = cfg.Defaults.Standard
		meta.Profile = activeProfileName(profileReq, cfg)
		meta.Mode = modeLabel(exp, enforcing)
		meta.Enforcing = enforcing
		meta.Redact = cfg.Auth != nil
	}
	meta.Failure = failure
	if err := writeStructured(formatName, out, &runner.Run{}, meta, func(runner.Result) bool { return false }); err != nil {
		fmt.Fprintln(os.Stderr, "report error:", err)
	}
}

// reportFiles is the default file each report format writes to. text is not
// here: it is always printed to stdout.
var reportFiles = map[string]string{
	"html":  "frostfall-report.html",
	"sarif": "frostfall.sarif",
	"json":  "frostfall.json",
	"junit": "frostfall-junit.xml",
}

// structuredMeta is the run context for the json and junit formats. baseURL
// must already be redacted (reportedBase).
func structuredMeta(toolVersion, axeVersion, baseURL, standard, profile, mode string, started time.Time, exitStatus int) format.ReportMeta {
	return format.ReportMeta{
		ToolVersion: toolVersion,
		AxeVersion:  axeVersion,
		BaseURL:     baseURL,
		Standard:    standard,
		Profile:     profile,
		Mode:        mode,
		Started:     started,
		Duration:    time.Since(started),
		ExitStatus:  exitStatus,
	}
}

// writeStructured writes the json or junit report to out (or its default
// file) and says where.
func writeStructured(formatName, out string, res *runner.Run, meta format.ReportMeta, flagged func(runner.Result) bool) error {
	if out == "" {
		out = reportFiles[formatName]
	}
	err := writeReportFile(out, func(w io.Writer) error {
		if formatName == "junit" {
			return format.JUnit(w, res, meta)
		}
		return format.JSON(w, res, meta, flagged)
	})
	if err != nil {
		return err
	}
	fmt.Printf("report written to %s\n", out)
	return nil
}

// writeReportFile writes out the way os.Create always has (truncate and
// write in place, so a symlink is written through, the file keeps its mode,
// owner and links, and a device or pipe such as /dev/stdout works), with one
// addition: a render, write or close that fails never leaves a partial report
// that could contradict the exit code. When out names the regular file itself
// it is removed; when it reaches a regular file through a symlink the target
// is emptied and the link kept; a device or pipe is left alone.
func writeReportFile(out string, render func(io.Writer) error) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	err = render(f)
	fi, serr := f.Stat()
	regular := serr == nil && fi.Mode().IsRegular()
	ownName := false // out is this file's own name, not a symlink to it
	if regular {
		li, lerr := os.Lstat(out)
		ownName = lerr == nil && os.SameFile(li, fi)
	}
	if err != nil && regular && !ownName {
		f.Truncate(0) // through the open file, so only the file written is touched
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil && ownName {
		os.Remove(out)
	}
	return err
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
	if _, isReport := reportFiles[formatName]; isReport {
		if outputPath == "" {
			outputPath = reportFiles[formatName]
		}
		fmt.Fprintf(f, "report-file=%s\n", outputPath)
	}
}

// syncIssues reconciles GitHub issues against this run's failing violations —
// the shared enforcement predicate, so filed issues always match what the
// exit code and reports flag. Close scoping uses the checks that actually
// ran on each page — a configured test whose check never ran must not close
// its issues as "fixed".
func syncIssues(res *runner.Run, flagged func(runner.Result) bool, dryRun bool) error {
	var failing []runner.Result
	for _, r := range res.Results {
		if flagged(r) {
			failing = append(failing, r)
		}
	}
	groups := github.BuildGroups(failing)
	covered := res.Covered

	if dryRun {
		// Without credentials we cannot see existing issues, so a dry run
		// plans against an empty set: it shows the full desired state.
		for _, a := range github.Plan(groups, nil, covered) {
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
	actions, err := github.Sync(client, groups, existing, covered, runContext)
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

// reportedBase is the base URL as a report may show it. With auth configured it
// is redacted (userinfo, query, params, fragment): the report header would
// otherwise print ciuser:password@host straight into a file that gets
// uploaded. Without auth it is unchanged.
func reportedBase(base string, cfg *config.Config) string {
	if cfg.Auth != nil {
		return browser.RedactURL(base)
	}
	return base
}

// knownBaseURL is the base URL a run would use when it can be known without
// starting a server: --base-url, else the config's baseUrl. "" for serve mode,
// whose origin is an ephemeral port.
func knownBaseURL(cfg *config.Config, baseURLFlag, serveFlag string) string {
	switch {
	case baseURLFlag != "":
		return baseURLFlag
	case serveFlag != "", cfg.Server == nil:
		return ""
	}
	return cfg.Server.BaseURL
}

// discoverAuthWarning says what a crawl costs when the session cannot be
// shared: with auth.reuse: false every page it loads runs the login flow, up
// to discover.maxPages of them. "" when it does not apply.
func discoverAuthWarning(cfg *config.Config) string {
	if cfg.Auth == nil || cfg.Auth.Setup == nil || cfg.Auth.ReuseSetup() {
		return ""
	}
	max := 100
	if cfg.Discover != nil && cfg.Discover.MaxPages > 0 {
		max = cfg.Discover.MaxPages
	}
	return fmt.Sprintf("auth.reuse is false, so discovery logs in once per page it loads, up to discover.maxPages (%d) loads, "+
		"and each discovered test logs in again when it runs", max)
}

// newCrawler builds the crawler a --discover run uses, so a dry run crawls
// by exactly the same rules.
func newCrawler(b *browser.Browser, authn *browser.Auth, base string, cfg *config.Config) *discover.Crawler {
	opts := config.Discover{MaxDepth: 3, MaxPages: 100}
	if cfg.Discover != nil {
		opts = *cfg.Discover
	}
	return &discover.Crawler{
		Browser:  b,
		Auth:     authn,
		BaseURL:  base,
		Options:  opts,
		Defaults: cfg.Defaults,
		Log:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
}

// discoverTests crawls from the root and synthesizes fresh-load tests for
// pages no explicit test covers. Explicit tests always win on a path
// collision - discovery adds breadth, never overrides depth.
func discoverTests(b *browser.Browser, authn *browser.Auth, base string, cfg *config.Config, explicit []config.Test) ([]config.Test, error) {
	paths, err := newCrawler(b, authn, base, cfg).Run()
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

// runDryRun crawls and prints what it found, one path per line on stdout,
// sorted, and runs nothing else. The paths are what a test's path: takes, so a
// line can be pasted into the config as it is. Diagnostics, and how many of the
// pages no configured test covers, go to stderr, which keeps stdout clean to
// diff or pipe. Coverage is judged against every test in the config, not the
// ones --id or --path select: those narrow what a run scans, and this scans
// nothing. A test with an absolute url: instead of a path: covers no page, as
// in a normal discovery run.
func runDryRun(b *browser.Browser, authn *browser.Auth, base string, cfg *config.Config) int {
	paths, err := newCrawler(b, authn, base, cfg).Run()
	haltIfInterrupted()
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover error:", err)
		return exitEnvironment
	}
	out, uncovered, skipped := dryRunListing(paths, cfg.Auth != nil, cfg.Tests)
	for _, p := range out {
		fmt.Println(p)
	}
	if skipped > 0 {
		fmt.Fprintln(os.Stderr, dryRunSkippedLine(skipped, cfg.Auth != nil))
	}
	fmt.Fprintf(os.Stderr, "discovered %d page(s), %d not covered by an explicit test\n", len(out), uncovered)
	return exitOK
}

// dryRunSkippedLine says how many paths a dry run left out, and why, without
// naming any of them. Signed out the only reason is an unprintable character;
// signed in the character rule of signedInListable applies too.
func dryRunSkippedLine(n int, auth bool) string {
	if auth {
		return fmt.Sprintf("skipped %d path(s) that can't be listed safely (encoded characters, query-like characters, or unprintable characters)", n)
	}
	return fmt.Sprintf("skipped %d path(s) with unprintable characters (a newline would split the listing)", n)
}

// dryRunListing turns the crawl's paths into the sorted, deduped lines to
// print, and counts how many of those lines no test in tests covers.
//
// A path with an unprintable character in it would not print as the one line
// it is, signed in or out: a %0A in a link decodes to a real newline, a raw
// %9B to an 8-bit CSI byte a terminal acts on, and a bidi override (U+202E)
// or a line separator (U+2028) reorders or splits what is shown. Such a path
// is skipped, not listed or counted, and skipped says how many so the caller
// can report it.
//
// Signed in, the crawler cuts queries, ;params and hash payloads, but a link
// can hide a credential behind any number of encoding layers (%253F is a ?
// two decodes down), and no decoder can know when to stop. So the listing
// fails closed instead: a path is printed only if signedInListable accepts
// it, and every other path is skipped and counted the same way. The paths are
// not parsed as URLs again (that misreads a literal % as a bad escape and a
// leading // as a host); MaskSegments, which works on the text, still masks a
// segment that is a secret by shape, and a test's path is masked the same way
// before it is compared, so a test of a token-shaped page still covers its
// line. The count is of printed lines, after the dedupe, so it cannot exceed
// the number listed.
func dryRunListing(paths []string, auth bool, tests []config.Test) (lines []string, uncovered, skipped int) {
	covered := map[string]bool{}
	for _, t := range tests {
		if auth {
			covered[secrets.MaskSegments(t.Path)] = true
		} else {
			covered[t.Path] = true
		}
	}
	listed := map[string]bool{}
	for _, p := range paths {
		if unprintable(p) || (auth && !signedInListable(p)) {
			skipped++
			continue
		}
		if auth {
			p = secrets.MaskSegments(p)
		}
		if listed[p] {
			continue
		}
		listed[p] = true
		if !covered[p] {
			uncovered++
		}
		lines = append(lines, p)
	}
	sort.Strings(lines)
	return lines, uncovered, skipped
}

// unprintable reports whether p would not print as the single line it is:
// invalid UTF-8 (a raw 8-bit byte such as CSI), a control character, a format
// character (bidi overrides and marks, zero-width characters) or a line or
// paragraph separator.
func unprintable(p string) bool {
	if !utf8.ValidString(p) {
		return true
	}
	return strings.ContainsFunc(p, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.In(r, unicode.Zl, unicode.Zp)
	})
}

// signedInListable reports whether a signed-in dry run may print p: it holds
// none of the characters a credential is carried or encoded with (% ? ; & =),
// and no '#' unless it is a single '#' directly followed by '/', a hash
// route. This is a character rule, not a decoder, so it has no layer it can
// be talked past. It cannot see a plain secret in an ordinary segment
// (/a/s3cr3t); MaskSegments catches one shaped like a token.
func signedInListable(p string) bool {
	if strings.ContainsAny(p, "%?;&=") {
		return false
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		rest := p[i+1:]
		if !strings.HasPrefix(rest, "/") || strings.Contains(rest, "#") {
			return false
		}
	}
	return true
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

// resolveServer picks the server mode and returns the base URL, the teardown,
// and on failure the exit code and the message to print for it.
func resolveServer(cfg *config.Config, baseURLFlag, serveFlag string) (string, func(), int, string) {
	noop := func() {}
	switch {
	case baseURLFlag != "":
		return probeAttach(baseURLFlag, noop, cfg.Auth != nil)
	case serveFlag != "":
		return startStatic(serveFlag, true)
	case cfg.Server == nil:
		return "", noop, exitBadConfig, "no server configured: set server: in config or pass --base-url / --serve"
	case cfg.Server.BaseURL != "":
		return probeAttach(cfg.Server.BaseURL, noop, cfg.Auth != nil)
	case cfg.Server.Serve != "":
		spa := cfg.Server.SPAFallback == nil || *cfg.Server.SPAFallback
		return startStatic(cfg.Server.Serve, spa)
	default:
		return "", noop, exitEnvironment, "server.command (spawn mode) is not implemented yet; use --base-url or --serve"
	}
}

func probeAttach(url string, cleanup func(), redact bool) (string, func(), int, string) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || resp.StatusCode >= 500 {
		shown := url
		if redact {
			shown = browser.RedactURL(url) // userinfo in the base URL is a credential
		}
		return "", cleanup, exitEnvironment,
			fmt.Sprintf("cannot reach %s — is the server running? (or use --serve for a static build)", shown)
	}
	return url, cleanup, exitOK, ""
}

func startStatic(dir string, spa bool) (string, func(), int, string) {
	srv := &server.Static{Dir: dir, SPAFallback: spa}
	url, err := srv.Start()
	if err != nil {
		return "", func() {}, exitEnvironment, "serve error: " + err.Error()
	}
	return url, func() { srv.Stop() }, exitOK, ""
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
