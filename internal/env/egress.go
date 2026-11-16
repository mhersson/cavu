package env

import (
	"slices"
	"sort"
)

// toolchainHosts serve mise and the toolchains; only the setup phase reaches them.
var toolchainHosts = []string{
	"api.github.com", "dl.google.com", "github.com", "go.dev", "mise.jdx.dev",
	"nodejs.org", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
	"static.rust-lang.org",
}

// registryHosts are the package registries of each ecosystem; both phases reach them.
var registryHosts = map[string][]string{
	"go":     {"proxy.golang.org", "sum.golang.org"},
	"node":   {"registry.npmjs.org"},
	"python": {"files.pythonhosted.org", "pypi.org"},
	"rust":   {"crates.io", "index.crates.io", "static.crates.io"},
}

// SetupHosts are the toolchain downloads plus the registries of the ecosystems.
func SetupHosts(ecosystems []string) []string {
	return hosts(toolchainHosts, ecosystems)
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
