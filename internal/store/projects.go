package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Baseline is the result of an environment check on a project.
type Baseline struct {
	Commit string
	Passed bool
	At     time.Time
	Output string
}

// Project is a repository CAVU works on, with its commands and policy.
type Project struct {
	ID                          int64
	Name, RepoURL, BaseBranch   string
	SetupCommand, VerifyCommand string
	VerifyWaived                bool
	ToolOverrides               map[string]string // tool name -> version
	EgressHosts                 []string
	Env                         map[string]string
	SecretVars                  []string // variable names; values live in secrets as project.<name>.<VAR>
	Switches                    map[string]bool
	Caps                        map[string]string // the project's own task caps, by settings key
	Baseline                    *Baseline         // nil until an environment check ran
	CheckRequestedAt            *time.Time        // set while a check is asked for or running
	CheckResult                 CheckResult
	Forge                       ForgePolicy
	RemovedAt                   *time.Time // set by RemoveProject only, never by a write of the project
}

// ForgePolicy is a project's forge policy: whether it has no CI, the check
// names it ignores, and the logins whose review comments may start a follow-up run.
type ForgePolicy struct {
	NoCI                      bool
	IgnoredChecks, ReviewBots []string
}

// CheckResult is what the newest environment check that ended found.
type CheckResult struct {
	Tools, IgnoredTools            []string
	BaseCommit, FailedStep, Output string
	VerifySuggestion, Fault        string // Fault: a runner error's text
}

const projectColumns = `id, name, repo_url, base_branch, setup_command, verify_command,
	verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches,
	baseline_commit, baseline_passed, baseline_at, baseline_output,
	check_requested_at, check_result, caps, forge, removed_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(sc scanner) (Project, error) {
	var (
		p                                  Project
		tools, egress, env, secretVars, sw string
		checkResult, caps, forge           string
		bCommit, bOutput                   sql.NullString
		bPassed                            sql.NullBool
		bAt, checkAt, removedAt            sql.NullInt64
	)

	if err := sc.Scan(&p.ID, &p.Name, &p.RepoURL, &p.BaseBranch, &p.SetupCommand, &p.VerifyCommand,
		&p.VerifyWaived, &tools, &egress, &env, &secretVars, &sw,
		&bCommit, &bPassed, &bAt, &bOutput, &checkAt, &checkResult, &caps, &forge, &removedAt); err != nil {
		return Project{}, err
	}

	for _, f := range []struct {
		src string
		dst any
	}{
		{tools, &p.ToolOverrides},
		{egress, &p.EgressHosts},
		{env, &p.Env},
		{secretVars, &p.SecretVars},
		{sw, &p.Switches},
		{checkResult, &p.CheckResult},
		{caps, &p.Caps},
		{forge, &p.Forge},
	} {
		if err := json.Unmarshal([]byte(f.src), f.dst); err != nil {
			return Project{}, fmt.Errorf("decode project %d: %w", p.ID, err)
		}
	}

	if bAt.Valid {
		p.Baseline = &Baseline{
			Commit: bCommit.String,
			Passed: bPassed.Bool,
			At:     time.UnixMilli(bAt.Int64),
			Output: bOutput.String,
		}
	}

	if checkAt.Valid {
		at := time.UnixMilli(checkAt.Int64)
		p.CheckRequestedAt = &at
	}

	if removedAt.Valid {
		at := time.UnixMilli(removedAt.Int64)
		p.RemovedAt = &at
	}

	return p, nil
}

// projectArgs returns the column values in the order of the insert statement.
func projectArgs(p Project) ([]any, error) {
	var enc [8]string

	for i, v := range []any{p.ToolOverrides, p.EgressHosts, p.Env, p.SecretVars, p.Switches, p.CheckResult, p.Caps, p.Forge} {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode project: %w", err)
		}

		enc[i] = string(b)
	}

	var bCommit, bOutput, bPassed, bAt any
	if b := p.Baseline; b != nil {
		bCommit, bPassed, bAt, bOutput = b.Commit, b.Passed, b.At.UnixMilli(), b.Output
	}

	var checkAt any
	if p.CheckRequestedAt != nil {
		checkAt = p.CheckRequestedAt.UnixMilli()
	}

	return []any{
		p.Name, p.RepoURL, p.BaseBranch, p.SetupCommand, p.VerifyCommand, p.VerifyWaived,
		enc[0], enc[1], enc[2], enc[3], enc[4],
		bCommit, bPassed, bAt, bOutput, checkAt, enc[5], enc[6], enc[7],
	}, nil
}

// CreateProject inserts p and returns it with its id. A duplicate name returns ErrConflict.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	args, err := projectArgs(p)
	if err != nil {
		return Project{}, err
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO projects (name, repo_url, base_branch,
		setup_command, verify_command, verify_waived, tool_overrides, egress_hosts, env,
		secret_vars, switches, baseline_commit, baseline_passed, baseline_at, baseline_output,
		check_requested_at, check_result, caps, forge)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, fmt.Errorf("project name %q: %w", p.Name, ErrConflict)
		}

		return Project{}, fmt.Errorf("create project: %w", err)
	}

	p.ID, err = res.LastInsertId()
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}

	return p, nil
}

// UpdateProject rewrites the project with p.ID. A missing id returns ErrNotFound
// and a duplicate name returns ErrConflict.
func (s *Store) UpdateProject(ctx context.Context, p Project) error {
	args, err := projectArgs(p)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `UPDATE projects SET name = ?, repo_url = ?, base_branch = ?,
		setup_command = ?, verify_command = ?, verify_waived = ?, tool_overrides = ?,
		egress_hosts = ?, env = ?, secret_vars = ?, switches = ?, baseline_commit = ?,
		baseline_passed = ?, baseline_at = ?, baseline_output = ?, check_requested_at = ?,
		check_result = ?, caps = ?, forge = ? WHERE id = ?`, append(args, p.ID)...)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("project name %q: %w", p.Name, ErrConflict)
		}

		return fmt.Errorf("update project: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}

func (s *Store) projectWhere(ctx context.Context, where, live string, arg any) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE `+where+` = ?`+live, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}

	return p, err
}

// Project returns the project with id, a removed one too, or ErrNotFound.
func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	return s.projectWhere(ctx, "id", "", id)
}

// ProjectByName returns the live project called name, or ErrNotFound.
func (s *Store) ProjectByName(ctx context.Context, name string) (Project, error) {
	return s.projectWhere(ctx, "name", " AND removed_at IS NULL", name)
}

// Projects returns every live project ordered by name.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE removed_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project

	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// ProjectNames returns every project's name by id, removed ones included, so
// a removed project's finished tasks still show it.
func (s *Store) ProjectNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("project names: %w", err)
	}
	defer rows.Close()

	out := map[int64]string{}

	for rows.Next() {
		var (
			id   int64
			name string
		)

		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("project names: %w", err)
		}

		out[id] = name
	}

	return out, rows.Err()
}

// RemoveProject marks the live project id removed at the given time and deletes
// every secret whose name starts with secretPrefix, in one transaction. A
// removed or unknown id returns ErrNotFound.
func (s *Store) RemoveProject(ctx context.Context, id int64, at time.Time, secretPrefix string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE projects SET removed_at = ? WHERE id = ? AND removed_at IS NULL`, at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	if err := rowsAffectedOrNotFound(res); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE substr(name, 1, length(?1)) = ?1`, secretPrefix); err != nil {
		return fmt.Errorf("remove project secrets: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	return nil
}
