package engine

import (
	"fmt"
	"maps"
	"time"
)

// The settings that bound what a task uses since a person last acted.
const (
	keyTaskTimeout = "task_timeout_minutes"
	keyTaskTurns   = "task_max_turns"
)

// capKeys are the settings a project may cap for its own tasks.
var capKeys = []string{keyTaskTimeout, keyTaskTurns}

// Limits are a task's budget since the last human action.
type Limits struct {
	Turns       int
	SessionTime time.Duration
}

// readLimits reads a task's limits from the settings with its project's caps over them.
func readLimits(settings, caps map[string]string) (Limits, error) {
	values := maps.Clone(settings)
	maps.Copy(values, caps)

	set := settingsReader{values: values}
	l := set.limits()

	return l, set.err
}

// overBudget is the stop of a task that used its turns or its session time
// since a person last acted.
func overBudget(s Snapshot) (Action, bool) {
	switch {
	case s.Facts.Turns >= s.Limits.Turns:
		return halt(StopBudget, ClearRetry, "The task used its turns.", turnsUsed(s)), true
	case s.Facts.SessionTime >= s.Limits.SessionTime:
		return halt(StopTimeout, ClearRetry, "The task used its session time.", timeUsed(s)), true
	}

	return Action{}, false
}

func turnsUsed(s Snapshot) string {
	return fmt.Sprintf("The task used %d of its %d turns since a person last acted.", s.Facts.Turns, s.Limits.Turns)
}

func timeUsed(s Snapshot) string {
	return fmt.Sprintf("The task's sessions ran %.0f of its %.0f minutes since a person last acted.",
		s.Facts.SessionTime.Minutes(), s.Limits.SessionTime.Minutes())
}

// left is what a session may use of the task's budget; overBudget keeps both above zero.
func left(s Snapshot) (turns int, d time.Duration) {
	return s.Limits.Turns - s.Facts.Turns, s.Limits.SessionTime - s.Facts.SessionTime
}
