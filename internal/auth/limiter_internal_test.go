package auth

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLimiterPrunesIdleEntries(t *testing.T) {
	t.Parallel()

	clock := time.Unix(1000, 0)
	l := NewLimiter(func() time.Time { return clock })

	for i := range limiterPruneAbove + 100 {
		l.Failure(fmt.Sprintf("user%d|ip", i))
	}

	clock = clock.Add(2 * time.Hour)

	l.Failure("trigger|prune")

	l.mu.Lock()
	defer l.mu.Unlock()

	require.Len(t, l.entries, 1)
}
