package sidecar

import "github.com/mhersson/cavu/internal/proxy"

// Request is the one JSON document on standard input.
type Request struct {
	Op    string // "proxy" or "clone"
	Proxy *ProxyRequest
	Clone *CloneRequest
}

// ProxyRequest configures the proxy op.
type ProxyRequest struct {
	Socket     string // /handoff/proxy.sock
	AllowHosts []string
	Model      *proxy.ModelRoute
	CABundle   string // PEM
}

// CloneRequest configures the clone op. Token is the forge token.
type CloneRequest struct {
	URL, Token, BaseBranch, Commit, Branch string // Commit "" = tip of BaseBranch
	MirrorDir, WorkspaceDir                string
	CABundle                               string
	Name, Email                            string // commit identity configured in the workspace
}

// Line is one JSON line on standard output.
type Line struct {
	Type   string       // "ready", "denied", "usage", "model_error", "cloned", "error"
	Host   string       `json:",omitempty"`
	Usage  *proxy.Usage `json:",omitempty"`
	Status int          `json:",omitempty"`
	Kind   string       `json:",omitempty"`
	Commit string       `json:",omitempty"` // cloned: the base commit
	Error  string       `json:",omitempty"`
}
