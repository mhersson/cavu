package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// projectJSON is a project as the API shows it. Its lists and maps are
// copies that are never nil, so they go out as [] and {}, never null.
type projectJSON struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	RepoURL       string            `json:"repo_url"`
	BaseBranch    string            `json:"base_branch"`
	SetupCommand  string            `json:"setup_command"`
	VerifyCommand string            `json:"verify_command"`
	VerifyWaived  bool              `json:"verify_waived"`
	ToolOverrides map[string]string `json:"tool_overrides"`
	EgressHosts   []string          `json:"egress_hosts"`
	Env           map[string]string `json:"env"`
	SecretVars    []string          `json:"secret_vars"`
	Switches      map[string]bool   `json:"switches"`
	Runnable      bool              `json:"runnable"`
	Baseline      *baselineJSON     `json:"baseline"`
	Check         checkJSON         `json:"check"`
}

type baselineJSON struct {
	Commit string    `json:"commit"`
	Passed bool      `json:"passed"`
	At     time.Time `json:"at"`
	Output string    `json:"output"`
}

type checkJSON struct {
	RequestedAt      *time.Time `json:"requested_at"`
	Tools            []string   `json:"tools"`
	IgnoredTools     []string   `json:"ignored_tools"`
	FailedStep       string     `json:"failed_step"`
	VerifySuggestion string     `json:"verify_suggestion"`
	Fault            string     `json:"fault"`
	Commit           string     `json:"commit"`
	Output           string     `json:"output"`
}

func projectView(p store.Project) projectJSON {
	c := p.CheckResult
	v := projectJSON{
		ID: p.ID, Name: p.Name, RepoURL: p.RepoURL, BaseBranch: p.BaseBranch,
		SetupCommand: p.SetupCommand, VerifyCommand: p.VerifyCommand, VerifyWaived: p.VerifyWaived,
		ToolOverrides: maps.Collect(maps.All(p.ToolOverrides)), EgressHosts: append([]string{}, p.EgressHosts...),
		Env: maps.Collect(maps.All(p.Env)), SecretVars: append([]string{}, p.SecretVars...),
		Switches: engine.ProjectSwitches(p), Runnable: engine.Runnable(p),
		Check: checkJSON{
			Tools: append([]string{}, c.Tools...), IgnoredTools: append([]string{}, c.IgnoredTools...),
			FailedStep: c.FailedStep, VerifySuggestion: c.VerifySuggestion, Fault: c.Fault, Commit: c.BaseCommit, Output: c.Output,
		},
	}

	if b := p.Baseline; b != nil {
		v.Baseline = &baselineJSON{Commit: b.Commit, Passed: b.Passed, At: b.At.UTC(), Output: b.Output}
	}

	if at := p.CheckRequestedAt; at != nil {
		v.Check.RequestedAt = new(at.UTC())
	}

	return v
}

// projectFields are what a request may set on a project. A field left out, or
// null, keeps its value.
type projectFields struct {
	RepoURL       *string            `json:"repo_url"`
	BaseBranch    *string            `json:"base_branch"`
	SetupCommand  *string            `json:"setup_command"`
	VerifyCommand *string            `json:"verify_command"`
	VerifyWaived  *bool              `json:"verify_waived"`
	ToolOverrides *map[string]string `json:"tool_overrides"`
	EgressHosts   *[]string          `json:"egress_hosts"`
	Env           *map[string]string `json:"env"`
	Switches      map[string]bool    `json:"switches"` // merged per key
	Secrets       map[string]string  `json:"secrets"`  // variable to value; an empty value removes the variable
}

// apply writes the fields over p. What p may hold is the engine's to refuse.
func (f projectFields) apply(p *store.Project) {
	set(&p.RepoURL, f.RepoURL)
	set(&p.BaseBranch, f.BaseBranch)
	set(&p.SetupCommand, f.SetupCommand)
	set(&p.VerifyCommand, f.VerifyCommand)
	set(&p.VerifyWaived, f.VerifyWaived)
	set(&p.ToolOverrides, f.ToolOverrides)
	set(&p.EgressHosts, f.EgressHosts)
	set(&p.Env, f.Env)

	sw := map[string]bool{}
	maps.Copy(sw, p.Switches)
	maps.Copy(sw, f.Switches)
	p.Switches = sw

	// A variable keeps its place, since the baseline rests on the list as it is.
	for _, name := range slices.Sorted(maps.Keys(f.Secrets)) {
		switch i := slices.Index(p.SecretVars, name); {
		case f.Secrets[name] == "" && i >= 0:
			p.SecretVars = slices.Delete(p.SecretVars, i, i+1)
		case f.Secrets[name] != "" && i < 0:
			p.SecretVars = append(p.SecretVars, name)
		}
	}
}

func set[T any](dst, v *T) {
	if v != nil {
		*dst = *v
	}
}

// secretPrefix starts the name of each of a project's secrets, where a run reads them.
func secretPrefix(p store.Project) string {
	return "project." + p.Name + "."
}

func (s *server) projects(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.Store.Projects(r.Context())
	if err != nil {
		return err
	}

	out := make([]projectJSON, len(ps))
	for i, p := range ps {
		out[i] = projectView(p)
	}

	reply(w, http.StatusOK, map[string][]projectJSON{"projects": out})

	return nil
}

// createProject stores the project before it seals its secrets, so a refused project seals none.
func (s *server) createProject(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
		projectFields
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	p := store.Project{Name: in.Name, BaseBranch: "main"}
	in.apply(&p)

	p, err := engine.AddProject(r.Context(), s.Store, p)
	if err != nil {
		return err
	}

	if err := s.seal(r.Context(), secretPrefix(p), in.Secrets); err != nil {
		return err
	}

	reply(w, http.StatusCreated, projectView(p))

	return nil
}

func (s *server) project(w http.ResponseWriter, r *http.Request) error {
	return s.replyProject(w, r, http.StatusOK)
}

// editProject has no name to change: a project's secrets are sealed under it.
func (s *server) editProject(w http.ResponseWriter, r *http.Request) error {
	var in projectFields
	if err := decode(w, r, &in); err != nil {
		return err
	}

	p, err := s.stored(r)
	if err != nil {
		return err
	}

	in.apply(&p)

	if err := engine.EditProject(r.Context(), s.Store, p); err != nil {
		return err
	}

	if err := s.seal(r.Context(), secretPrefix(p), in.Secrets); err != nil {
		return err
	}

	return s.replyProject(w, r, http.StatusOK)
}

func (s *server) requestCheck(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	if err := s.Loop.RequestCheck(r.Context(), id); err != nil {
		return err
	}

	s.Nudge()

	return s.replyProject(w, r, http.StatusAccepted)
}

// stored reads the project the path names.
func (s *server) stored(r *http.Request) (store.Project, error) {
	id, err := pathID(r)
	if err != nil {
		return store.Project{}, err
	}

	p, err := s.Store.Project(r.Context(), id)
	if err != nil {
		return store.Project{}, fmt.Errorf("project %d: %w", id, err)
	}

	return p, nil
}

// replyProject answers with the project the path names, as stored.
func (s *server) replyProject(w http.ResponseWriter, r *http.Request, status int) error {
	p, err := s.stored(r)
	if err != nil {
		return err
	}

	reply(w, status, projectView(p))

	return nil
}
