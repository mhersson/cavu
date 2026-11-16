package engine

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		return RunConfig{}, fmt.Errorf("run config: project %d: %w", t.ProjectID, err)
	}

	sw, err := resolveSwitches(p, overrides)
	if err != nil {
		return RunConfig{}, fmt.Errorf("run config: %w", err)
	}

	all, err := l.Store.Settings(ctx)
	if err != nil {
		return RunConfig{}, fmt.Errorf("run config: %w", err)
	}

	values := map[string]string{}
	maps.Copy(values, all)

	for _, k := range []string{keyModelWork, keyEffortWork} {
		if v, ok := overrides[k]; ok {
			values[k] = v
		}
	}

	set := settingsReader{values: values}

	var c RunConfig

	c.TaskID, c.RunID, c.ProjectID = t.ID, r.ID, p.ID
	c.InstanceID, c.Image = set.required("instance_id"), l.Image
	c.Repo.URL, c.Repo.BaseBranch, c.Repo.BaseCommit, c.Repo.Branch = p.RepoURL, p.BaseBranch, t.BaseCommit, BranchName(t.Title)
	c.Setup.Command, c.Setup.Env, c.Setup.ToolOverrides, c.Setup.EgressHosts = p.SetupCommand, p.Env, p.ToolOverrides, p.EgressHosts
	c.Verify.Command, c.Verify.Waived = p.VerifyCommand, p.VerifyWaived
	c.Verify.Timeout = set.minutes("verify_timeout_minutes", 10, 0.5, 120)

	if b := p.Baseline; b != nil {
		c.Verify.BaselineCommit, c.Verify.BaselinePassed = b.Commit, b.Passed
	}

	c.Endpoint.Mode = set.oneOf("endpoint_mode", true, "subscription", "key")
	c.Endpoint.URL = values["endpoint_url"]
	c.Endpoint.AuthHeader = set.oneOf("endpoint_auth_header", false, "bearer", "x-api-key")
	c.Agent.Model, c.Agent.Effort = set.required(keyModelWork), values[keyEffortWork]
	c.Agent.MaxTurns = set.positiveInt("work_max_turns", 200)
	c.Agent.Timeout = time.Duration(set.positiveInt("work_timeout_minutes", 60)) * time.Minute
	c.Agent.WebTools = sw.WebTools
	c.Identity.Name, c.Identity.Email = set.required("commit_name"), set.required("commit_email")
	c.Task.Title, c.Task.Body = t.Title, t.Body

	if path := values["ca_bundle_path"]; path != "" {
		if c.CABundle, err = os.ReadFile(filepath.Clean(path)); err != nil {
			set.fail("ca_bundle_path", err.Error())
		}
	}

	if set.err != nil {
		return RunConfig{}, fmt.Errorf("run config: %w", set.err)
	}

	if err := l.readSecrets(ctx, p, &c); err != nil {
		return RunConfig{}, fmt.Errorf("run config: %w", err)
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

// resolveSwitches applies a project's switches over the defaults, then a task's
// overrides. The snapshot and the run config both resolve through it.
func resolveSwitches(p store.Project, overrides map[string]string) (Switches, error) {
	sw := Switches{Continuations: true, VerifyFix: true}
	fields := map[string]*bool{"continuations": &sw.Continuations, "verify_fix": &sw.VerifyFix, "web_tools": &sw.WebTools}

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

func (s *settingsReader) fail(key, problem string) {
	if s.err == nil {
		s.err = fmt.Errorf("setting %s: %s", key, problem)
	}
}

func (s *settingsReader) required(key string) string {
	v := s.values[key]
	if v == "" {
		s.fail(key, "not set")
	}

	return v
}

func (s *settingsReader) oneOf(key string, required bool, allowed ...string) string {
	v := s.values[key]

	switch {
	case v == "" && required:
		s.fail(key, "not set")
	case v != "" && !slices.Contains(allowed, v):
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
