// Package config loads and validates the .frostfall.yml schema.
//
// Unknown keys are an error, not a warning: a typo in a11y config means
// silently not scanning, which is worse than failing loudly (exit code 2).
package config

import (
	"cmp"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aquia-inc/frostfall/internal/keyboard"
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
	Server   *Server          `yaml:"server"`
	Defaults *ProfileDefaults `yaml:"defaults"`
	Baseline string           `yaml:"baseline"`
	Discover *Discover        `yaml:"discover"`
}

// ProfileDefaults mirrors Defaults with pointer fields so an overlay can
// distinguish "absent" from "zero": a profile can now RELAX enforcement
// (expect: {} clears the base contract) and override any single default,
// which value-typed fields keyed on zero checks could not express.
type ProfileDefaults struct {
	Standard   *string         `yaml:"standard"`
	Viewport   *Viewport       `yaml:"viewport"`
	WaitFor    *string         `yaml:"waitFor"`
	SettleTime *Duration       `yaml:"settleTime"`
	Timeout    *Duration       `yaml:"timeout"`
	Rules      map[string]bool `yaml:"rules"`
	Expect     *Expect         `yaml:"expect"`
	Keyboard   *KeyboardStep   `yaml:"keyboard"`
}

// UnmarshalYAML: a bare `keyboard:` means on, as everywhere else.
func (d *ProfileDefaults) UnmarshalYAML(node *yaml.Node) error {
	type plain ProfileDefaults
	if err := decodeStrict(node, (*plain)(d)); err != nil {
		return err
	}
	defaultNulls(node, map[string]func(){
		"keyboard": func() { d.Keyboard = cmp.Or(d.Keyboard, &KeyboardStep{}) },
	})
	return nil
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
	// Keyboard audits every test's initial state, discovered ones included,
	// unless a test sets its own keyboard (false opts it out). Opt-in per
	// config, never a binary default: turning it on in a release would fail
	// every enforcing pipeline on findings nobody asked for.
	Keyboard *KeyboardStep `yaml:"keyboard"`
}

// UnmarshalYAML: a bare `keyboard:` means on, as everywhere else.
func (d *Defaults) UnmarshalYAML(node *yaml.Node) error {
	type plain Defaults
	if err := decodeStrict(node, (*plain)(d)); err != nil {
		return err
	}
	defaultNulls(node, map[string]func(){
		"keyboard": func() { d.Keyboard = cmp.Or(d.Keyboard, &KeyboardStep{}) },
	})
	return nil
}

// KeyboardFor is the keyboard audit t's initial state gets, nil for none: the
// test's own setting when it has one, else the default.
func (c *Config) KeyboardFor(t Test) *KeyboardStep {
	k := t.Keyboard
	if k == nil {
		k = c.Defaults.Keyboard
	}
	if k == nil || k.Off {
		return nil
	}
	return k
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
	// Keyboard opts the initial state into a keyboard audit.
	Keyboard *KeyboardStep `yaml:"keyboard"`
	// Transcript records the initial state's screen reader transcript.
	Transcript *TranscriptStep `yaml:"transcript"`
}

// Step is one entry of the step vocabulary (DESIGN.md §4). Exactly one action
// field is set.
type Step struct {
	Click   string    `yaml:"click"`
	Fill    Fields    `yaml:"fill"`
	Press   string    `yaml:"press"`
	Hover   string    `yaml:"hover"`
	Select  Fields    `yaml:"select"`
	WaitFor string    `yaml:"waitFor"`
	Wait    Duration  `yaml:"wait"`
	Scan    *ScanStep `yaml:"scan"`
	Goto    string    `yaml:"goto"`
	// Keyboard audits the state the preceding steps reached.
	Keyboard *KeyboardStep `yaml:"keyboard"`
	// Transcript records what a screen reader would announce now.
	Transcript *TranscriptStep `yaml:"transcript"`
	// ExpectFocus asserts where focus is now.
	ExpectFocus *ExpectFocus `yaml:"expectFocus"`
}

// Field is one selector/value pair of a fill or select step.
type Field struct {
	Selector string
	Value    string
}

// Fields preserves the config's document order. The YAML syntax is an
// ordinary mapping; decoding into a Go map would randomize execution order,
// making dependent fields (a select that repopulates another) flaky.
type Fields []Field

func (f *Fields) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping of selector: value", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		// Merge keys are rejected deliberately: merge-in-place and ordered
		// execution conflict (does a merged duplicate re-run or replace?),
		// and letting one through would either produce a confusing decode
		// error or a literal "<<" CSS selector at runtime.
		if key := node.Content[i]; key.Tag == "!!merge" || key.Value == "<<" {
			return fmt.Errorf("line %d: merge keys (<<) are not supported in fill/select; list the fields explicitly", key.Line)
		}
		var sel, val string
		if err := node.Content[i].Decode(&sel); err != nil {
			return err
		}
		if err := node.Content[i+1].Decode(&val); err != nil {
			return err
		}
		*f = append(*f, Field{Selector: sel, Value: val})
	}
	return nil
}

// decodeStrict decodes node into v, rejecting unknown keys. A custom
// UnmarshalYAML must use it instead of node.Decode, which does not inherit
// the outer decoder's KnownFields: a typo'd scope would otherwise be dropped
// and the scan would silently cover the whole page. The keys are checked
// against v's yaml tags rather than by re-decoding a marshalled copy, which
// would lose anchors defined elsewhere in the file.
func decodeStrict(node *yaml.Node, v any) error {
	if err := checkKeys(node, reflect.TypeOf(v).Elem()); err != nil {
		return err
	}
	return node.Decode(v)
}

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// checkKeys rejects mapping keys with no yaml-tagged field in t, recursing
// into struct fields that have no UnmarshalYAML of their own (those check
// themselves), so a nested block like expectFocus is covered too.
func checkKeys(node *yaml.Node, t reflect.Type) error {
	if node.Kind != yaml.MappingNode || t.Kind() != reflect.Struct {
		return nil
	}
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "-" {
			fields[name] = t.Field(i).Type
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Tag == "!!merge" {
			continue
		}
		ft, ok := fields[key.Value]
		if !ok {
			return fmt.Errorf("line %d: field %s not found", key.Line, key.Value)
		}
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if !reflect.PointerTo(ft).Implements(unmarshalerType) {
			if err := checkKeys(node.Content[i+1], ft); err != nil {
				return err
			}
		}
	}
	return nil
}

// UnmarshalYAML gives a null `scan:`, `keyboard:`, or `transcript:` step its
// documented meaning, all defaults.
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	type plain Step
	if err := decodeStrict(node, (*plain)(s)); err != nil {
		return err
	}
	defaultNulls(node, map[string]func(){
		"scan":       func() { s.Scan = cmp.Or(s.Scan, &ScanStep{}) },
		"keyboard":   func() { s.Keyboard = cmp.Or(s.Keyboard, &KeyboardStep{}) },
		"transcript": func() { s.Transcript = cmp.Or(s.Transcript, &TranscriptStep{}) },
	})
	return nil
}

// defaultNulls runs fill for each key of the mapping whose value is null,
// following aliases and merge keys (<<) as the decoder does. yaml.v3 never
// calls a field's own UnmarshalYAML for null; it leaves the pointer nil,
// which reads as "not configured" rather than the documented defaults. Each
// fill leaves an already-decoded value alone, so an explicit key still wins
// over a merged one.
func defaultNulls(node *yaml.Node, fill map[string]func()) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if val.Kind == yaml.AliasNode {
			val = val.Alias
		}
		if key.Tag == "!!merge" || key.Value == "<<" {
			merged := []*yaml.Node{val}
			if val.Kind == yaml.SequenceNode {
				merged = val.Content
			}
			for _, m := range merged {
				defaultNulls(m, fill)
			}
			continue
		}
		if f, ok := fill[key.Value]; ok && val.Tag == "!!null" {
			f()
		}
	}
}

// decodeToggle decodes a block that also takes a bare boolean, in any form
// the rule toggles accept (true/false, on/off, yes/no): off sets *off, on
// means all defaults.
func decodeToggle(node *yaml.Node, v any, off *bool) error {
	if node.Kind == yaml.ScalarNode && node.Tag != "!!null" {
		var on bool
		if err := node.Decode(&on); err != nil {
			return fmt.Errorf("line %d: want true, false, or a mapping: %w", node.Line, err)
		}
		*off = !on
		return nil
	}
	return decodeStrict(node, v)
}

// ScanStep supports both bare `- scan:` / `scan: {}` and the full form.
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
	return decodeStrict(node, (*plain)(s))
}

// UnmarshalYAML gives a bare `keyboard:` or `transcript:` on a test the
// meaning the same key has on a step - on - rather than silently skipping
// the check the config asked for.
func (t *Test) UnmarshalYAML(node *yaml.Node) error {
	type plain Test
	if err := decodeStrict(node, (*plain)(t)); err != nil {
		return err
	}
	defaultNulls(node, map[string]func(){
		"keyboard":   func() { t.Keyboard = cmp.Or(t.Keyboard, &KeyboardStep{}) },
		"transcript": func() { t.Transcript = cmp.Or(t.Transcript, &TranscriptStep{}) },
	})
	return nil
}

// KeyboardStep configures a keyboard traversal audit. As a test field it
// accepts true/false or the full form (without label: it is the initial
// state); as a step, bare `- keyboard:` or the full form.
type KeyboardStep struct {
	Label   string          `yaml:"label"`
	Scope   string          `yaml:"scope"`
	MaxTabs int             `yaml:"maxTabs"`
	Rules   map[string]bool `yaml:"rules"`
	// Off records an explicit `keyboard: false`.
	Off bool `yaml:"-"`
}

func (k *KeyboardStep) UnmarshalYAML(node *yaml.Node) error {
	type plain KeyboardStep
	return decodeToggle(node, (*plain)(k), &k.Off)
}

func (k *KeyboardStep) validate() error {
	if k.MaxTabs < 0 {
		return fmt.Errorf("keyboard.maxTabs: must not be negative")
	}
	if err := validateRules("keyboard.rules", k.Rules); err != nil {
		return err
	}
	return onlyRules("keyboard.rules", k.Rules, true)
}

// onlyRules rejects ids the block's check never reads: a keyboard step's
// rules only reach the keyboard audit, a scan step's only axe, so a toggle
// for the other would validate and silently do nothing. Defaults and test
// rules feed both, so they take either.
func onlyRules(where string, rules map[string]bool, kb bool) error {
	for id := range rules {
		// expectFocus reads only the defaults' and test's rules.
		if kb && id == keyboard.RuleFocusExpectation {
			return fmt.Errorf("%s: %q applies to expectFocus steps; set it in the test's rules", where, id)
		}
		if keyboard.IsRule(id) != kb {
			if kb {
				return fmt.Errorf("%s: %q is an axe rule; set it in a scan step or the test's rules", where, id)
			}
			return fmt.Errorf("%s: %q is a frostfall/ rule; set it in a keyboard step or the test's rules", where, id)
		}
	}
	return nil
}

// validateRules rejects frostfall/ rule ids this binary does not implement:
// a typo there would silently leave a keyboard check on. Axe ids are not
// checked here - axe itself rejects unknown ones at scan time.
func validateRules(where string, rules map[string]bool) error {
	for id := range rules {
		if keyboard.IsRule(id) && !keyboard.Known(id) {
			return fmt.Errorf("%s: unknown rule %q", where, id)
		}
	}
	return nil
}

// TranscriptStep records a screen reader transcript. Like KeyboardStep, the
// test-level form takes true/false or the full form without a label.
type TranscriptStep struct {
	Label string `yaml:"label"`
	Scope string `yaml:"scope"`
	// Off records an explicit `transcript: false`.
	Off bool `yaml:"-"`
}

func (t *TranscriptStep) UnmarshalYAML(node *yaml.Node) error {
	type plain TranscriptStep
	return decodeToggle(node, (*plain)(t), &t.Off)
}

// ExpectFocus asserts where focus is: every field set must hold. Role and
// name are the accessible role and name a screen reader would announce.
type ExpectFocus struct {
	Label  string `yaml:"label"`
	Within string `yaml:"within"`
	Role   string `yaml:"role"`
	Name   string `yaml:"name"`
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
		if d.Standard != nil {
			c.Defaults.Standard = *d.Standard
		}
		if d.Viewport != nil {
			c.Defaults.Viewport = *d.Viewport
		}
		if d.WaitFor != nil {
			c.Defaults.WaitFor = *d.WaitFor
		}
		if d.SettleTime != nil {
			c.Defaults.SettleTime = *d.SettleTime
		}
		if d.Timeout != nil {
			c.Defaults.Timeout = *d.Timeout
		}
		if d.Rules != nil {
			c.Defaults.Rules = d.Rules
		}
		// A present expect replaces the base wholesale — including an empty
		// one, which is how a profile relaxes an enforcing base to
		// report-only (e.g. a "local" profile under an enforcing default).
		if d.Expect != nil {
			c.Defaults.Expect = *d.Expect
		}
		if d.Keyboard != nil {
			c.Defaults.Keyboard = d.Keyboard
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
	if d.Viewport.Width == 0 && d.Viewport.Height == 0 {
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
	// Half-set viewports were previously clobbered back to the default
	// silently; refuse them instead.
	if (c.Defaults.Viewport.Width == 0) != (c.Defaults.Viewport.Height == 0) {
		return fmt.Errorf("defaults.viewport: width and height must both be set")
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
	if err := validateRules("defaults.rules", c.Defaults.Rules); err != nil {
		return err
	}
	if k := c.Defaults.Keyboard; k != nil {
		if k.Label != "" || k.Scope != "" {
			return fmt.Errorf("defaults.keyboard: label and scope are per test; set them on a keyboard step")
		}
		if err := k.validate(); err != nil {
			return fmt.Errorf("defaults: %w", err)
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
		if err := validateRules("rules", t.Rules); err != nil {
			return fmt.Errorf("test %q: %w", t.ID, err)
		}
		if tr := t.Transcript; tr != nil && tr.Label != "" {
			return fmt.Errorf("test %q: transcript.label: the test-level transcript is always labelled initial; use a transcript step to name one", t.ID)
		}
		if k := t.Keyboard; k != nil {
			if k.Label != "" {
				return fmt.Errorf("test %q: keyboard.label: the test-level audit is always labelled initial; use a keyboard step to name one", t.ID)
			}
			if err := k.validate(); err != nil {
				return fmt.Errorf("test %q: %w", t.ID, err)
			}
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
		s.Keyboard != nil, s.Transcript != nil, s.ExpectFocus != nil,
	} {
		if set {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("each step must have exactly one action")
	}
	if s.Scan != nil {
		if err := validateRules("scan.rules", s.Scan.Rules); err != nil {
			return err
		}
		if err := onlyRules("scan.rules", s.Scan.Rules, false); err != nil {
			return err
		}
	}
	if t := s.Transcript; t != nil && t.Off {
		return fmt.Errorf("transcript: false is only meaningful on a test; drop the step instead")
	}
	if e := s.ExpectFocus; e != nil && e.Within == "" && e.Role == "" && e.Name == "" {
		return fmt.Errorf("expectFocus: set at least one of within, role, name")
	}
	if k := s.Keyboard; k != nil {
		if k.Off {
			return fmt.Errorf("keyboard: false is only meaningful on a test; drop the step instead")
		}
		return k.validate()
	}
	return nil
}
