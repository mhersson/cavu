package main

import (
	"context"
	"fmt"
	"io"

	"github.com/mhersson/cavu/internal/doctor"
	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
)

func init() {
	commands["doctor"] = command{
		summary: "doctor: check the runtime, the image, the settings, the endpoint and the forge",
		run:     runDoctorVerb,
	}
}

// runDoctor is the seam the unit tests replace: the real one drives containers.
var runDoctor = doctor.Run

func runDoctorVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	f := newVerbFlags("cavu doctor [--data-dir DIR]", stderr)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	// The checks run under run and task id 0, as a project check does.
	if err := a.lock(roleVerb); a.serverHolds(err) {
		return fmt.Errorf("a cavu server is running on %s; the doctor runs only while none is", a.dir)
	} else if err != nil {
		return err
	}

	d, err := a.doctorDeps(ctx)
	if err != nil {
		return err
	}

	// doctor.Run masked each check; nothing here adds to what it prints.
	checks := runDoctor(ctx, d)
	for _, c := range checks {
		printCheckLine(stdout, c)
	}

	if doctor.ExitCode(checks) != 0 {
		return exitError{code: 1}
	}

	return nil
}

func (a *app) doctorDeps(ctx context.Context) (doctor.Deps, error) {
	settings, err := a.store.Settings(ctx)
	if err != nil {
		return doctor.Deps{}, err
	}

	f, bundle, err := a.forgeClient(ctx)
	if err != nil {
		return doctor.Deps{}, err
	}

	d := doctor.Deps{
		Version:      version,
		AgentVersion: claudecode.Version,
		Settings:     settings,
		CABundle:     bundle,
		Forge:        f,
		Driver:       claudecode.New(),
	}

	if d.Credential, _, err = a.vault.Get(ctx, engine.SecretEndpointCredential); err != nil {
		return doctor.Deps{}, err
	}

	if d.ForgeToken, _, err = a.vault.Get(ctx, engine.SecretForgeToken); err != nil {
		return doctor.Deps{}, err
	}

	if d.ForgeAppKey, _, err = a.vault.Get(ctx, engine.SecretForgeAppKey); err != nil {
		return doctor.Deps{}, err
	}

	ps, err := a.store.Projects(ctx)
	if err != nil {
		return doctor.Deps{}, err
	}

	for _, p := range ps {
		d.Projects = append(d.Projects, doctor.Project{
			ID: p.ID, Name: p.Name, RepoURL: p.RepoURL, BaseBranch: p.BaseBranch, Publish: engine.ProjectSwitches(p)["publish"],
		})
	}

	return d, nil
}

func printCheckLine(w io.Writer, c doctor.Check) {
	status := "FAIL"

	switch {
	case c.Skipped:
		status = "skipped"
	case c.Warning:
		status = "warning"
	case c.OK:
		status = "ok"
	}

	fmt.Fprintf(w, "%-12s %-8s %s\n", c.Name, status, c.Detail)

	if c.Fix != "" && (!c.OK || c.Warning) {
		fmt.Fprintf(w, "%-12s %-8s fix: %s\n", "", "", c.Fix)
	}
}
