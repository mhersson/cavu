package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
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

// BuildRunConfig reads the project, settings and secrets. overrides (may be nil)
// replace model_work, effort_work and switches for a replay.
func (l *Loop) BuildRunConfig(ctx context.Context, t store.Task, r store.Run, overrides map[string]string) (RunConfig, error) {
	p, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return RunConfig{}, fmt.Errorf("project %d: %w", t.ProjectID, err)
	}

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

func (l *Loop) readSecrets(ctx context.Context, p store.Project, c *RunConfig) error {
	var err error

	if c.Repo.Token, _, err = l.Vault.Get(ctx, "forge_token"); err != nil {
		return err
	}

	if c.Endpoint.Credential, _, err = l.Vault.Get(ctx, "endpoint_credential"); err != nil {
		return err
	}

	c.Setup.Secrets = make(map[string]string, len(p.SecretVars))

	for _, v := range p.SecretVars {
		name := "project." + p.Name + "." + v

		val, ok, err := l.Vault.Get(ctx, name)
		if err != nil {
			return err
		}

		if !ok {
			return fmt.Errorf("secret %s is not set", name)
		}

		c.Setup.Secrets[v] = val
	}

	return nil
}

// ReadSettings reads the part of a run's config the settings hold: the
// instance id, the endpoint but its credential, the agent but its web tools,
// the identity and the verify timeout. Its error names every required setting
// not set, else the first value that does not parse.
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
	"work_timeout_minutes", "work_max_turns", "verify_timeout_minutes",
	"commit_name", "commit_email", "concurrency", "listen_address",
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
	set.read()

	_, concurrency := Concurrency(values)
	_, listen := ListenAddress(values)

	return errors.Join(set.err, concurrency, listen)
}

// Concurrency reads the concurrency setting: a positive integer, 3 when unset.
func Concurrency(values map[string]string) (int, error) {
	set := settingsReader{values: values}
	n := set.positiveInt("concurrency", 3)

	return n, set.err
}

// ListenAddress reads the listen_address setting: a host and a port, 127.0.0.1:7117 when unset.
func ListenAddress(values map[string]string) (string, error) {
	set := settingsReader{values: values}
	addr := set.hostPort("listen_address", "127.0.0.1:7117")

	return addr, set.err
}

// switchFields maps each switch's name to its field in sw.
func switchFields(sw *Switches) map[string]*bool {
	return map[string]*bool{
		"continuations": &sw.Continuations, "verify_fix": &sw.VerifyFix, "web_tools": &sw.WebTools, "readiness": &sw.Readiness,
		"review": &sw.Review,
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
	sw := Switches{Continuations: true, VerifyFix: true, Readiness: true}
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
	values map[string]string
	err    error
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
	c.Agent.MaxTurns = s.positiveInt("work_max_turns", 200)
	c.Agent.Timeout = time.Duration(s.positiveInt("work_timeout_minutes", 60)) * time.Minute
	c.Verify.Timeout = s.minutes("verify_timeout_minutes", 10, 0.5, 120)
	c.Identity.Name, c.Identity.Email = s.values["commit_name"], s.values["commit_email"]

	return c
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

func (s *settingsReader) positiveInt(key string, def int) int {
	v, ok := s.values[key]
	if !ok || v == "" {
		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		s.fail(key, fmt.Sprintf("%q is not a positive integer", v))
	}

	return n
}

func (s *settingsReader) hostPort(key, def string) string {
	v := cmp.Or(s.values[key], def)
	if host, port, err := net.SplitHostPort(v); err != nil || host == "" || port == "" {
		s.fail(key, fmt.Sprintf("%q is not a host and a port", v))
	}

	return v
}

// minutes reads a decimal number of minutes, clamped to [lo, hi].
func (s *settingsReader) minutes(key string, def, lo, hi float64) time.Duration {
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
