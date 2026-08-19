// Package config loads and validates the .frostfall.yml schema.
//
// Unknown keys are an error, not a warning: a typo in a11y config means
// silently not scanning, which is worse than failing loudly (exit code 2).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration for "60s"/"500ms" YAML values.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Version  int                `yaml:"version"`
	// Name identifies the app in report headers. Optional: when empty it is
	// detected from CI env, the git remote, or package.json.
	Name     string             `yaml:"name"`
	Server   *Server            `yaml:"server"`
	Defaults Defaults           `yaml:"defaults"`
	Auth     *Auth              `yaml:"auth"`
	Baseline string             `yaml:"baseline"`
	Discover *Discover          `yaml:"discover"`
	Profiles map[string]Profile `yaml:"profiles"`
	Tests    []Test             `yaml:"tests"`
}

// Profile is a named overlay for environment differences (local vs CI). It
// deliberately has no Tests field: one test list serves every environment, so
// coverage can never silently fork between local runs and CI. A profile
// carrying a tests key fails strict decoding.
type Profile struct {
	Server   *Server   `yaml:"server"`
	Defaults *Defaults `yaml:"defaults"`
	Baseline string    `yaml:"baseline"`
	Discover *Discover `yaml:"discover"`
}

type Server struct {
	// Mode 2: spawn.
	Command      string            `yaml:"command"`
	Port         int               `yaml:"port"`
	ReadyWhen    string            `yaml:"readyWhen"` // httpOk | portOpen | logMatch
	ReadyPattern string            `yaml:"readyPattern"`
	ReadyPath    string            `yaml:"readyPath"`
	Timeout      Duration          `yaml:"timeout"`
	Env          map[string]string `yaml:"env"`
	// Mode 3: static.
	Serve       string `yaml:"serve"`
	SPAFallback *bool  `yaml:"spaFallback"`
	// Mode 1: attach.
	BaseURL string `yaml:"baseUrl"`
}

type Viewport struct {
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
}

type Defaults struct {
	Standard   string          `yaml:"standard"`
	Viewport   Viewport        `yaml:"viewport"`
	WaitFor    string          `yaml:"waitFor"`
	SettleTime Duration        `yaml:"settleTime"`
	Timeout    Duration        `yaml:"timeout"`
	Rules      map[string]bool `yaml:"rules"`
	Expect     Expect          `yaml:"expect"`
}

// Expect is the enforcement contract. When no expect block is configured
// anywhere, Frostfall is report-only: violations are reported but never fail
// the run. Setting severity (or rules) opts into failing CI.
type Expect struct {
	Severity      string   `yaml:"severity"`
	MaxViolations *int     `yaml:"maxViolations"`
	// Rules fails on new violations of these rule ids regardless of impact —
	// for teams that enforce a curated set before adopting a severity floor.
	Rules []string `yaml:"rules"`
}

// Enforcing reports whether this expect block opts into failing the build.
func (e Expect) Enforcing() bool {
	return e.Severity != "" || e.MaxViolations != nil || len(e.Rules) > 0
}

// normalizeExpect fills the implied fields of an enforcing expect: severity
// with no maxViolations means zero tolerance, and maxViolations with neither
// severity nor rules implies the serious floor — otherwise nothing would ever
// count against the limit and the config would enforce vacuously.
func normalizeExpect(e *Expect) {
	if e.MaxViolations != nil && e.Severity == "" && len(e.Rules) == 0 {
		e.Severity = "serious"
	}
	if e.Severity != "" && e.MaxViolations == nil {
		zero := 0
		e.MaxViolations = &zero
	}
}

type Auth struct {
	Setup        *AuthSetup `yaml:"setup"`
	Reuse        *bool      `yaml:"reuse"`
	StorageState string     `yaml:"storageState"`
}

type AuthSetup struct {
	Path  string `yaml:"path"`
	Steps []Step `yaml:"steps"`
}

type Discover struct {
	MaxDepth int      `yaml:"maxDepth"`
	Exclude  []string `yaml:"exclude"`
	MaxPages int      `yaml:"maxPages"`
}

type Test struct {
	ID      string          `yaml:"id"`
	Path    string          `yaml:"path"`
	URL     string          `yaml:"url"`
	Scan    *bool           `yaml:"scan"`
	WaitFor string          `yaml:"waitFor"`
	Steps   []Step          `yaml:"steps"`
	Rules   map[string]bool `yaml:"rules"`
	Expect  *Expect         `yaml:"expect"`
}

// Step is one entry of the step vocabulary (DESIGN.md §4). Exactly one action
// field is set.
type Step struct {
	Click   string            `yaml:"click"`
	Fill    map[string]string `yaml:"fill"`
	Press   string            `yaml:"press"`
	Hover   string            `yaml:"hover"`
	Select  map[string]string `yaml:"select"`
	WaitFor string            `yaml:"waitFor"`
	Wait    Duration          `yaml:"wait"`
	Scan    *ScanStep         `yaml:"scan"`
	Goto    string            `yaml:"goto"`
}

// ScanStep supports both bare `- scan` / `scan: {}` and the full form.
type ScanStep struct {
	Label string          `yaml:"label"`
	Scope string          `yaml:"scope"`
	Rules map[string]bool `yaml:"rules"`
}

func (s *ScanStep) UnmarshalYAML(node *yaml.Node) error {
	// `scan: true` and bare `- scan` (null) mean all defaults.
	switch node.Tag {
	case "!!bool", "!!null":
		return nil
	}
	type plain ScanStep
	return node.Decode((*plain)(s))
}

var validStandards = map[string]bool{
	"wcag2a": true, "wcag2aa": true, "wcag21a": true,
	"wcag21aa": true, "wcag22aa": true, "section508": true,
	"best-practice": true,
}

var validSeverities = map[string]bool{
	"minor": true, "moderate": true, "serious": true, "critical": true,
}

// ProfileAuto requests the "ci" profile when it exists and no profile
// otherwise — the resolution used when running under CI with no explicit
// --profile. ProfileNone forces the base config even in CI.
const (
	ProfileAuto = "\x00auto"
	ProfileNone = "none"
)

// Load reads, interpolates, strictly decodes, applies the requested profile
// overlay, applies defaults, and validates. profile is "" or ProfileNone for
// the base config, ProfileAuto for CI auto-selection, or an explicit name
// (which must exist).
func Load(path, profile string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	interpolated, err := interpolate(string(raw))
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(strings.NewReader(interpolated))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.applyProfile(profile); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// applyProfile merges the selected overlay onto the base config. Blocks
// override as units (server, discover, rules, expect); scalar defaults
// override field-wise where the profile sets them. Precedence overall:
// base config, then profile, then command-line flags.
func (c *Config) applyProfile(profile string) error {
	switch profile {
	case "", ProfileNone:
		return nil
	case ProfileAuto:
		if _, ok := c.Profiles["ci"]; !ok {
			return nil
		}
		profile = "ci"
	default:
		if _, ok := c.Profiles[profile]; !ok {
			return fmt.Errorf("profile %q not defined in config", profile)
		}
	}
	p := c.Profiles[profile]

	if p.Server != nil {
		c.Server = p.Server
	}
	if p.Baseline != "" {
		c.Baseline = p.Baseline
	}
	if p.Discover != nil {
		c.Discover = p.Discover
	}
	if d := p.Defaults; d != nil {
		if d.Standard != "" {
			c.Defaults.Standard = d.Standard
		}
		if d.Viewport.Width != 0 {
			c.Defaults.Viewport = d.Viewport
		}
		if d.WaitFor != "" {
			c.Defaults.WaitFor = d.WaitFor
		}
		if d.SettleTime != 0 {
			c.Defaults.SettleTime = d.SettleTime
		}
		if d.Timeout != 0 {
			c.Defaults.Timeout = d.Timeout
		}
		if d.Rules != nil {
			c.Defaults.Rules = d.Rules
		}
		if d.Expect.Enforcing() {
			c.Defaults.Expect = d.Expect
		}
	}
	return nil
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// interpolate expands ${VAR} and ${VAR:-default}; an unset var without a
// default is an error.
func interpolate(s string) (string, error) {
	var missing []string
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := envRe.FindStringSubmatch(m)
		name, hasDefault, def := groups[1], groups[2] != "", groups[3]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if hasDefault {
			return def
		}
		missing = append(missing, name)
		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	d := &c.Defaults
	if d.Standard == "" {
		d.Standard = "wcag21aa"
	}
	if d.Viewport.Width == 0 {
		d.Viewport = Viewport{Width: 1280, Height: 800}
	}
	if d.WaitFor == "" {
		d.WaitFor = "networkIdle"
	}
	if d.SettleTime == 0 {
		d.SettleTime = Duration(500 * time.Millisecond)
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(30 * time.Second)
	}
	// No expect defaults: an absent expect block means report-only. A set
	// severity with no maxViolations means zero tolerance at that severity.
	normalizeExpect(&d.Expect)
	for i := range c.Tests {
		if c.Tests[i].Expect != nil {
			normalizeExpect(c.Tests[i].Expect)
		}
	}
	if c.Server != nil {
		s := c.Server
		if s.Command != "" {
			if s.ReadyWhen == "" {
				s.ReadyWhen = "httpOk"
			}
			if s.ReadyPath == "" {
				s.ReadyPath = "/"
			}
			if s.Timeout == 0 {
				s.Timeout = Duration(60 * time.Second)
			}
		}
	}
	if c.Discover != nil {
		if c.Discover.MaxDepth == 0 {
			c.Discover.MaxDepth = 3
		}
		if c.Discover.MaxPages == 0 {
			c.Discover.MaxPages = 100
		}
	}
}

func (c *Config) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", c.Version)
	}
	if c.Server != nil {
		modes := 0
		for _, set := range []bool{c.Server.Command != "", c.Server.Serve != "", c.Server.BaseURL != ""} {
			if set {
				modes++
			}
		}
		if modes != 1 {
			return fmt.Errorf("server: exactly one of command, serve, baseUrl must be set")
		}
		switch c.Server.ReadyWhen {
		case "", "httpOk", "portOpen", "logMatch":
		default:
			return fmt.Errorf("server.readyWhen: must be httpOk, portOpen, or logMatch")
		}
		if c.Server.ReadyWhen == "logMatch" {
			if _, err := regexp.Compile(c.Server.ReadyPattern); c.Server.ReadyPattern == "" || err != nil {
				return fmt.Errorf("server.readyPattern: required valid Go regex with readyWhen: logMatch")
			}
		}
	}
	if !validStandards[c.Defaults.Standard] {
		return fmt.Errorf("defaults.standard: unknown standard %q", c.Defaults.Standard)
	}
	// Silently ignoring an auth block would mean scanning a login redirect
	// and passing — the worst failure mode for this tool. Refuse until auth
	// is implemented.
	if c.Auth != nil {
		return fmt.Errorf("auth: not implemented yet — remove the auth block (planned; see DESIGN.md)")
	}
	if c.Discover != nil {
		for _, pat := range c.Discover.Exclude {
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("discover.exclude %q: %w", pat, err)
			}
		}
	}
	if s := c.Defaults.Expect.Severity; s != "" && !validSeverities[s] {
		return fmt.Errorf("defaults.expect.severity: must be minor, moderate, serious, or critical")
	}
	if len(c.Tests) == 0 && c.Discover == nil {
		return fmt.Errorf("no tests defined and discovery not configured")
	}
	seen := map[string]bool{}
	for i, t := range c.Tests {
		if t.ID == "" {
			return fmt.Errorf("tests[%d]: id is required", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("tests[%d]: duplicate id %q", i, t.ID)
		}
		seen[t.ID] = true
		if (t.Path == "") == (t.URL == "") {
			return fmt.Errorf("test %q: exactly one of path, url must be set", t.ID)
		}
		if t.Expect != nil && t.Expect.Severity != "" && !validSeverities[t.Expect.Severity] {
			return fmt.Errorf("test %q: invalid expect.severity", t.ID)
		}
		for j, s := range t.Steps {
			if err := s.validate(); err != nil {
				return fmt.Errorf("test %q steps[%d]: %w", t.ID, j, err)
			}
		}
	}
	return nil
}

func (s *Step) validate() error {
	actions := 0
	for _, set := range []bool{
		s.Click != "", len(s.Fill) > 0, s.Press != "", s.Hover != "",
		len(s.Select) > 0, s.WaitFor != "", s.Wait != 0, s.Scan != nil, s.Goto != "",
	} {
		if set {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("each step must have exactly one action")
	}
	return nil
}
