package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RemoveOrphans removes every container with this instance's label, and every
// run volume of the instance whose run id is not in keepRuns. It selects by
// label, never by name, and never removes a volume without a run label: a
// task's home, a project's cache and mirror.
func RemoveOrphans(ctx context.Context, r Runtime, instanceID string, keepRuns []int64) error {
	if !instanceIDText.MatchString(instanceID) {
		return errors.New("sandbox: instance id: want 16 lower-case hex characters")
	}

	containers, volumes, err := r.List(ctx, instanceLabel+"="+instanceID)
	if err != nil {
		return fmt.Errorf("sandbox: orphans: %w", err)
	}

	// The name check holds against a runtime that ignores the label filter.
	containers = slices.DeleteFunc(containers, func(n string) bool { return !strings.HasPrefix(n, namePrefix(instanceID)) })

	if err := r.Remove(ctx, containers...); err != nil {
		return fmt.Errorf("sandbox: orphans: %w", err)
	}

	var orphans []string

	for _, v := range volumes {
		labels, err := r.VolumeLabels(ctx, v)
		if err != nil {
			return fmt.Errorf("sandbox: orphans: %w", err)
		}

		run, err := strconv.ParseInt(labels[runLabel], 10, 64)
		if labels[instanceLabel] != instanceID || err != nil || slices.Contains(keepRuns, run) {
			continue
		}

		orphans = append(orphans, v)
	}

	if err := r.VolumeRemove(ctx, orphans...); err != nil {
		return fmt.Errorf("sandbox: orphans: %w", err)
	}

	return nil
}
