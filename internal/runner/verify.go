package runner

import (
	"context"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
)

// Verify runs the verify command on the frozen work as a command of its own,
// in an agent container started afresh, so nothing the session left running
// survives to change what the gate sees.
func (r *Runner) Verify(ctx context.Context, c engine.RunConfig) (engine.VerifyResult, error) {
	if c.Verify.Waived {
		return engine.VerifyResult{Result: engine.ResultWaived}, nil
	}

	return act(ctx, r, c, func(ctx context.Context, st *run, mask func(string) string) (engine.VerifyResult, error) {
		if err := st.restartAgent(ctx); err != nil {
			return engine.VerifyResult{}, err
		}

		res, err := runVerify(ctx, st, c)
		res.Output = proc.HeadTail(mask(res.Output), proc.CaptureCap)

		return res, err
	})
}

// restartAgent replaces the agent container with a fresh one on the same
// volumes, so nothing a session or an earlier command left running survives.
func (st *run) restartAgent(ctx context.Context) error {
	st.agent = nil

	a, err := st.sb.Restart(ctx)
	if err != nil {
		return err
	}

	st.agent = a

	return nil
}
