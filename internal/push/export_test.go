package push

import "context"

// Pass runs one of n's passes.
func (n *Notifier) Pass(ctx context.Context) { n.pass(ctx) }
