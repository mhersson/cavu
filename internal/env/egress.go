package env

import (
	"slices"
	"sort"
)

// toolchainHosts serve mise and the toolchains; only the setup phase reaches them.
var toolchainHosts = []string{
	"api.github.com", "dl.google.com", "github.com", "go.dev", "mise-versions.jdx.dev", "mise.jdx.dev",
	"nodejs.org", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
	"sh.rustup.rs", "static.rust-lang.org", "tuf-repo-cdn.sigstore.dev",
}

// registryHosts are the package registries of each ecosystem; every phase reaches them.
var registryHosts = map[string][]string{
	"go":     {"proxy.golang.org", "sum.golang.org"},
	"node":   {"registry.npmjs.org"},
	"python": {"files.pythonhosted.org", "pypi.org"},
	"rust":   {"crates.io", "index.crates.io", "static.crates.io"},
}

// setupOnlyHosts are reached by the setup phase alone: the Go proxy redirects
// large modules to general storage, which the agent phase must not reach.
var setupOnlyHosts = map[string][]string{
	"go": {"storage.googleapis.com"},
}

// SetupHosts are the toolchain downloads plus the registries of the ecosystems
// and the hosts only setup reaches.
func SetupHosts(ecosystems []string) []string {
	out := hosts(toolchainHosts, ecosystems)
	for _, e := range ecosystems {
		out = append(out, setupOnlyHosts[e]...)
	}

	sort.Strings(out)

	return slices.Compact(out)
}

// AgentHosts are the registries of the ecosystems only.
func AgentHosts(ecosystems []string) []string {
	return hosts(nil, ecosystems)
}

func hosts(base, ecosystems []string) []string {
	out := slices.Clone(base)
	for _, e := range ecosystems {
		out = append(out, registryHosts[e]...)
	}

	sort.Strings(out)

	return slices.Compact(out)
}
