package sidecar

import "github.com/mhersson/cavu/internal/proxy"

// Request is the one JSON document on standard input. The body that is set
// names the op.
type Request struct {
	Proxy *ProxyRequest
	Clone *CloneRequest
}

// ProxyRequest configures the proxy op.
type ProxyRequest struct {
	Socket     string // /handoff/proxy.sock
	AllowHosts []string
	Model      *proxy.ModelRoute
	Budget     *proxy.Budget // nil: no money cap
}

// CloneRequest configures the clone op. Token is the forge token.
type CloneRequest struct {
	URL, Token, BaseBranch, Commit, Branch string // Commit "" = tip of BaseBranch
	MirrorDir, WorkspaceDir                string
	Name, Email                            string // commit identity configured in the workspace
}

// Line is one JSON line on standard output.
type Line struct {
	Type   string       // "ready", "denied", "usage", "budget", "cloned", "error"
	Host   string       `json:",omitempty"`
	Usage  *proxy.Usage `json:",omitempty"`
	Commit string       `json:",omitempty"` // cloned: the base commit
	Error  string       `json:",omitempty"` // budget: why the spend cannot be counted, "" when the cap is spent
}
