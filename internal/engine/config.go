package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Override keys a replay may set besides the switches.
const (
	keyModelWork  = "model_work"
	keyEffortWork = "effort_work"
)

// The settings that name the instance's files on the server's host, and the
// bound on the skills directory, the same as the adapter's on a repository's.
const (
	keySkillsPath       = "skills_path"
	keyInstructionsPath = "instructions_path"
	skillsBound         = 16 << 20
)

// BuildRunConfig reads the project, settings and secrets, and the instance's
// files for every action of a run. overrides (may be nil) replace model_work,
// effort_work and switches for a replay.
func (l *Loop) BuildRunConfig(ctx context.Context, t store.Task, r store.Run, overrides map[string]string) (RunConfig, error) {
	p, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return RunConfig{}, fmt.Errorf("project %d: %w", t.ProjectID, err)
	}

	c, err := l.runConfig(ctx, p, t, r, overrides)
	if err != nil {
		return RunConfig{}, err
	}

	// runConfig leaves the files out: an environment check runs no session.
	values, err := l.Store.Settings(ctx)
	if err != nil {
		return RunConfig{}, err
	}

	if file := values[keyInstructionsPath]; file != "" {
		data, err := os.ReadFile(file) //nolint:gosec // the file the operator named
		if err != nil {
			return RunConfig{}, fmt.Errorf("setting %s: %w", keyInstructionsPath, err)
		}

		c.Agent.Instance.Instructions = string(data)
	}

	if dir := values[keySkillsPath]; dir != "" {
		if c.Agent.Instance.Skills, err = readSkills(dir); err != nil {
			return RunConfig{}, fmt.Errorf("setting %s: %w", keySkillsPath, err)
		}
	}

	return c, nil
}

// forgeConfig builds the config of a call on the task's pull request as an
// environment check's is built, without the instance's files, since the watch
// looks every 10 seconds. runID is the open run's, 0 when none is open.
func (l *Loop) forgeConfig(ctx context.Context, t store.Task, runID int64, pr *PRFact) (RunConfig, error) {
	p, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return RunConfig{}, fmt.Errorf("project %d: %w", t.ProjectID, err)
	}

	c, err := l.runConfig(ctx, p, t, store.Run{ID: runID}, t.Overrides)
	c.Repo.Branch = pr.Branch

	return c, err
}

// readSkills reads every file below root but those in a .git directory,
// following links, by its slash path under root. The bound is checked
// before each read, so a link to a large tree fails early; a link loop ends
// in the system's path error.
func readSkills(root string) (map[string]File, error) {
	skills := map[string]File{}

	var (
		total int64
		walk  func(rel string) error
	)

	walk = func(rel string) error {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}

		for _, e := range entries {
			name := path.Join(rel, e.Name())
			p := filepath.Join(root, filepath.FromSlash(name))

			fi, err := os.Stat(p)
			if err != nil {
				return err
			}

			if fi.IsDir() {
				// A checkout's history is no part of a skill and can pass the bound alone.
				if e.Name() == ".git" {
					continue
				}

				if err := walk(name); err != nil {
					return err
				}

				continue
			}

			if total += fi.Size(); total > skillsBound {
				return fmt.Errorf("the files pass %d MiB at %s", skillsBound>>20, name)
			}

			data, err := os.ReadFile(p) //nolint:gosec // a file below the directory the operator named
			if err != nil {
				return err
			}

			skills[name] = File{Data: data, Executable: fi.Mode()&0o111 != 0}
		}

		return nil
	}

	return skills, walk("")
}

// runConfig builds BuildRunConfig's config from a project already read.
func (l *Loop) runConfig(ctx context.Context, p store.Project, t store.Task, r store.Run, overrides map[string]string) (RunConfig, error) {
	sw, err := resolveSwitches(p, overrides)
	if err != nil {
		return RunConfig{}, err
	}

	all, err := l.Store.Settings(ctx)
	if err != nil {
		return RunConfig{}, err
	}

	values := map[string]string{}
	maps.Copy(values, all)

	for _, k := range []string{keyModelWork, keyEffortWork} {
		if v, ok := overrides[k]; ok {
			values[k] = v
		}
	}

	c, err := ReadSettings(values)
	if err != nil {
		return RunConfig{}, err
	}

	c.TaskID, c.RunID, c.ProjectID, c.Image = t.ID, r.ID, p.ID, l.Image
	c.Repo.URL, c.Repo.BaseBranch, c.Repo.BaseCommit, c.Repo.Branch = p.RepoURL, p.BaseBranch, t.BaseCommit, BranchName(t.Title)
	c.Repo.IgnoredChecks, c.Repo.ReviewBots = p.Forge.IgnoredChecks, p.Forge.ReviewBots
	c.Setup.Command, c.Setup.Env, c.Setup.ToolOverrides, c.Setup.EgressHosts = p.SetupCommand, p.Env, p.ToolOverrides, p.EgressHosts
	c.Verify.Command, c.Verify.Waived = p.VerifyCommand, p.VerifyWaived

	if b := p.Baseline; b != nil {
		c.Verify.BaselineCommit, c.Verify.BaselinePassed = b.Commit, b.Passed
	}

	c.Agent.WebTools = sw.WebTools
	c.Task.Title, c.Task.Body = t.Title, t.Body

	if err := l.readSecrets(ctx, p, &c); err != nil {
		return RunConfig{}, err
	}

	return c, nil
}

// The names under which the engine reads the vault's global secrets.
const (
	// SecretForgeToken is the forge token.
	SecretForgeToken = "forge_token"
	// SecretForgeAppKey is the private key, PEM, of the GitHub App forge_app_id names.
	SecretForgeAppKey = "forge_app_key" //nolint:gosec // a name, not a credential
	// SecretEndpointCredential is the endpoint credential.
	SecretEndpointCredential = "endpoint_credential" //nolint:gosec // a name, not a credential
)

// ProjectSecretName is the vault name of the project's secret variable.
func ProjectSecretName(project, variable string) string {
	return "project." + project + "." + variable
}

func (l *Loop) readSecrets(ctx context.Context, p store.Project, c *RunConfig) error {
	var err error

	if c.Repo.Access.Token, _, err = l.Vault.Get(ctx, SecretForgeToken); err != nil {
		return err
	}

	if c.Repo.Access.AppID != 0 {
		if c.Repo.Access.AppKey, err = l.requiredSecret(ctx, SecretForgeAppKey); err != nil {
			return err
		}
	}

	if c.Endpoint.Credential, _, err = l.Vault.Get(ctx, SecretEndpointCredential); err != nil {
		return err
	}

	c.Setup.Secrets = make(map[string]string, len(p.SecretVars))

	for _, v := range p.SecretVars {
		if c.Setup.Secrets[v], err = l.requiredSecret(ctx, ProjectSecretName(p.Name, v)); err != nil {
			return err
		}
	}

	return nil
}

// requiredSecret reads a secret the run's config cannot be built without.
func (l *Loop) requiredSecret(ctx context.Context, name string) (string, error) {
	v, ok, err := l.Vault.Get(ctx, name)
	if err == nil && !ok {
		err = fmt.Errorf("secret %s is not set", name)
	}

	return v, err
}

// ReadSettings reads the part of a run's config the settings hold: the
// instance id, the endpoint but its credential, the agent's models and
// efforts, the identity, the verify timeout and the forge App id. Its error
// names every required setting not set, else the first value that does not
// parse.
func ReadSettings(values map[string]string) (RunConfig, error) {
	required := []string{"instance_id", "endpoint_mode", keyModelWork, "commit_name", "commit_email"}
	if values["endpoint_mode"] == "key" {
		// The proxy routes model calls to the URL and refuses an empty header.
		required = append(required, "endpoint_url", "endpoint_auth_header")
	}

	if missing := slices.DeleteFunc(required, func(k string) bool { return values[k] != "" }); len(missing) > 0 {
		return RunConfig{}, fmt.Errorf("settings not set: %s", strings.Join(missing, ", "))
	}

	set := settingsReader{values: values}

	c := set.read()
	if set.err != nil {
		return RunConfig{}, set.err
	}

	return c, nil
}

// settableKeys are the settings a person sets. instance_id is generated, and
// another value would orphan the instance's volumes.
var settableKeys = []string{
	"runtime", "ca_bundle_path", "endpoint_mode", "endpoint_url", "endpoint_auth_header",
	keyModelWork, keyEffortWork, "model_review", "effort_review", "model_strong", "effort_strong",
	keyTaskTimeout, keyTaskTurns, keyTaskBudget, keyDayBudget, keyPrices, "verify_timeout_minutes",
	"commit_name", "commit_email", "concurrency", "listen_address", keySkillsPath, keyInstructionsPath,
	"forge_app_id", "ci_timeout_minutes",
}

// SettableKeys lists the settings a person sets.
func SettableKeys() []string {
	return slices.Clone(settableKeys)
}

// CheckSetting refuses a key a person cannot set and a value a run or the
// server would not read. An empty value unsets the key.
func CheckSetting(key, value string) error {
	if !slices.Contains(settableKeys, key) {
		return fmt.Errorf("%s cannot be set; the settings are %s", key, strings.Join(settableKeys, ", "))
	}

	values := map[string]string{key: value}
	set := settingsReader{values: values}
	set.readAll()

	_, prices := Prices(values)

	return errors.Join(set.err, prices, checkInstancePath(key, value))
}

// SettingDefaults gives every settable key its default as text, "" for a key
// with none: one a run falls back on another key for, or one that is off unset.
func SettingDefaults() map[string]string {
	set := settingsReader{values: map[string]string{}}
	set.readAll()

	out := make(map[string]string, len(settableKeys))
	for _, k := range settableKeys {
		out[k] = set.defaults[k]
	}

	return out
}

// checkInstancePath refuses a skills_path that is not a directory and an
// instructions_path that is not a file; a link to one counts.
func checkInstancePath(key, value string) error {
	if value == "" || (key != keySkillsPath && key != keyInstructionsPath) {
		return nil
	}

	fi, err := os.Stat(value)

	switch {
	case err != nil:
		return fmt.Errorf("setting %s: %w", key, err)
	case key == keySkillsPath && !fi.IsDir():
		return fmt.Errorf("setting %s: %s is not a directory", key, value)
	case key == keyInstructionsPath && !fi.Mode().IsRegular():
		return fmt.Errorf("setting %s: %s is not a file", key, value)
	}

	return nil
}

// Concurrency reads the concurrency setting: a positive integer, 3 when unset.
func Concurrency(values map[string]string) (int, error) {
	set := settingsReader{values: values}
	n := set.concurrency()

	return n, set.err
}

// CITimeout reads how long a pushed commit's checks may stay unfinished: a
// whole number of minutes, 120 when unset; 0 means no limit and returns 0.
func CITimeout(values map[string]string) (time.Duration, error) {
	set := settingsReader{values: values}
	d := set.ciTimeout()

	return d, set.err
}

// ListenAddress reads the listen_address setting: a host and a port, 127.0.0.1:7117 when unset.
func ListenAddress(values map[string]string) (string, error) {
	set := settingsReader{values: values}
	addr := set.listenAddress()

	return addr, set.err
}

// switchFields maps each switch's name to its field in sw.
func switchFields(sw *Switches) map[string]*bool {
	return map[string]*bool{
		"continuations": &sw.Continuations, "verify_fix": &sw.VerifyFix, "web_tools": &sw.WebTools, "readiness": &sw.Readiness,
		"review": &sw.Review, "publish": &sw.Publish,
	}
}

// ProjectSwitches returns the state of each switch of a project, over the defaults.
func ProjectSwitches(p store.Project) map[string]bool {
	sw, _ := resolveSwitches(p, nil)
	out := map[string]bool{}

	for k, f := range switchFields(&sw) {
		out[k] = *f
	}

	return out
}

// resolveSwitches applies a project's switches over the defaults, then a task's
// overrides. The snapshot and the run config both resolve through it.
func resolveSwitches(p store.Project, overrides map[string]string) (Switches, error) {
	sw := Switches{Continuations: true, VerifyFix: true, Readiness: true, Review: true, Publish: true}
	fields := switchFields(&sw)

	for k, on := range p.Switches {
		if f, ok := fields[k]; ok {
			*f = on
		}
	}

	for k, v := range overrides {
		if k == keyModelWork || k == keyEffortWork {
			continue
		}

		f, ok := fields[k]
		if !ok {
			return Switches{}, fmt.Errorf("override %s is not a known key", k)
		}

		on, err := strconv.ParseBool(v)
		if err != nil {
			return Switches{}, fmt.Errorf("override %s: %w", k, err)
		}

		*f = on
	}

	return sw, nil
}

// settingsReader reads typed settings and keeps the first error, which names its key.
type settingsReader struct {
	values   map[string]string
	err      error
	defaults map[string]string // each key a reader gave a default, to its text
}

// readAll runs every reader that has a default, so CheckSetting and
// SettingDefaults see the same set.
func (s *settingsReader) readAll() {
	s.read()
	s.concurrency()
	s.listenAddress()
	s.ciTimeout()
}

func (s *settingsReader) concurrency() int { return s.wholeNumber("concurrency", 3, 1) }

func (s *settingsReader) ciTimeout() time.Duration {
	return time.Duration(s.wholeNumber("ci_timeout_minutes", 120, 0)) * time.Minute
}

func (s *settingsReader) listenAddress() string {
	return s.hostPort("listen_address", "127.0.0.1:7117")
}

// recordDefault notes a key's default; one below what the key takes is no
// value a person could set, so it is recorded as "".
func (s *settingsReader) recordDefault(key, text string) {
	if s.defaults == nil {
		s.defaults = map[string]string{}
	}

	s.defaults[key] = text
}

// read reads every setting a run uses; a key not in values takes its default.
func (s *settingsReader) read() RunConfig {
	var c RunConfig

	c.InstanceID = s.values["instance_id"]
	s.oneOf("runtime", "docker", "podman")
	c.Endpoint.Mode = s.oneOf("endpoint_mode", "subscription", "key")
	c.Endpoint.URL = s.values["endpoint_url"]
	c.Endpoint.AuthHeader = s.oneOf("endpoint_auth_header", "bearer", "x-api-key")
	c.Agent.Model, c.Agent.Effort = s.values[keyModelWork], s.values[keyEffortWork]
	c.Agent.Review.Model, c.Agent.Review.Effort = cmp.Or(s.values["model_review"], c.Agent.Model), cmp.Or(s.values["effort_review"], c.Agent.Effort)
	c.Agent.Strong.Model, c.Agent.Strong.Effort = cmp.Or(s.values["model_strong"], c.Agent.Model), cmp.Or(s.values["effort_strong"], c.Agent.Effort)
	// Read only to refuse a bad value: a session's turns and time are what is
	// left of the task's, which work and review set.
	s.limits()
	c.Verify.Timeout = s.minutes("verify_timeout_minutes", 10, 0.5, 120)
	c.Identity.Name, c.Identity.Email = s.values["commit_name"], s.values["commit_email"]
	c.Repo.Access.AppID = int64(s.wholeNumber("forge_app_id", 0, 1))

	return c
}

// limits reads the task's budget and the day's money; a session's config gets
// what is left of the turns and the time.
func (s *settingsReader) limits() Limits {
	return Limits{
		Turns: s.wholeNumber(keyTaskTurns, 1000, 1), SessionTime: time.Duration(s.wholeNumber(keyTaskTimeout, 300, 1)) * time.Minute,
		TaskMicroUSD: s.dollars(keyTaskBudget), DayMicroUSD: s.dollars(keyDayBudget),
	}
}

func (s *settingsReader) fail(key, problem string) {
	if s.err == nil {
		s.err = fmt.Errorf("setting %s: %s", key, problem)
	}
}

func (s *settingsReader) oneOf(key string, allowed ...string) string {
	v := s.values[key]
	if v != "" && !slices.Contains(allowed, v) {
		s.fail(key, fmt.Sprintf("%q is not one of %q", v, allowed))
	}

	return v
}

// wholeNumber reads a whole number of least or more.
func (s *settingsReader) wholeNumber(key string, def, least int) int {
	v := s.values[key]
	if v == "" {
		if def >= least {
			s.recordDefault(key, strconv.Itoa(def))
		} else {
			s.recordDefault(key, "")
		}

		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < least {
		s.fail(key, fmt.Sprintf("%q is not a whole number of %d or more", v, least))
	}

	return n
}

func (s *settingsReader) hostPort(key, def string) string {
	s.recordDefault(key, def)

	v := cmp.Or(s.values[key], def)
	if host, port, err := net.SplitHostPort(v); err != nil || host == "" || port == "" {
		s.fail(key, fmt.Sprintf("%q is not a host and a port", v))
	}

	return v
}

// dollars reads a decimal number of US dollars that is not negative, in
// micro-dollars; unset is 0.
func (s *settingsReader) dollars(key string) int64 {
	v := s.values[key]
	if v == "" {
		return 0
	}

	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f < 0 {
		s.fail(key, fmt.Sprintf("%q is not a number of dollars of 0 or more", v))

		return 0
	}

	return int64(math.Round(f * 1e6))
}

// minutes reads a decimal number of minutes, clamped to [lo, hi].
func (s *settingsReader) minutes(key string, def, lo, hi float64) time.Duration {
	s.recordDefault(key, strconv.FormatFloat(def, 'f', -1, 64))

	n := def

	if v, ok := s.values[key]; ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) {
			s.fail(key, fmt.Sprintf("%q is not a number of minutes", v))

			return 0
		}

		n = f
	}

	return time.Duration(min(max(n, lo), hi) * float64(time.Minute))
}
