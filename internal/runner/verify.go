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
		st.agent = nil

		a, err := st.sb.Restart(ctx)
		if err != nil {
			return engine.VerifyResult{}, err
		}

		st.agent = a

		res, err := runVerify(ctx, st, c)
		res.Output = proc.HeadTail(mask(res.Output), proc.CaptureCap)

		return res, err
	})
}
