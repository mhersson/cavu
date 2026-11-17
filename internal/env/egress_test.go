package env

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostListsSortedUniqueDisjoint(t *testing.T) {
	t.Parallel()

	lists := map[string][]string{"toolchain": toolchainHosts}
	maps.Copy(lists, registryHosts)

	for eco, l := range setupOnlyHosts {
		lists["setup-only "+eco] = l
	}

	for name, l := range lists {
		assert.IsIncreasing(t, l, name)
	}

	tc := map[string]bool{}
	for _, h := range toolchainHosts {
		tc[h] = true
	}

	for eco, l := range registryHosts {
		for _, h := range l {
			assert.False(t, tc[h], "%s shares %s with toolchain", eco, h)
		}
	}
}

func TestGoStorageHostIsSetupOnly(t *testing.T) {
	t.Parallel()

	assert.Contains(t, SetupHosts([]string{"go"}), "storage.googleapis.com")
	assert.NotContains(t, AgentHosts([]string{"go"}), "storage.googleapis.com")
	assert.NotContains(t, SetupHosts([]string{"node"}), "storage.googleapis.com")
	assert.IsIncreasing(t, SetupHosts([]string{"go"}))
}

func TestAgentHostsExcludeToolchainHosts(t *testing.T) {
	t.Parallel()

	all := []string{"go", "node", "python", "rust", "unknown"}
	agent := AgentHosts(all)
	setup := SetupHosts(all)

	for _, h := range toolchainHosts {
		assert.NotContains(t, agent, h)
		assert.Contains(t, setup, h)
	}

	assert.Contains(t, agent, "proxy.golang.org")
	assert.Contains(t, agent, "registry.npmjs.org")
	assert.IsIncreasing(t, agent)
	assert.IsIncreasing(t, setup)
}

func TestHostsOnlyGivenEcosystems(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"registry.npmjs.org"}, AgentHosts([]string{"node"}))
	assert.Empty(t, AgentHosts(nil))
	assert.Equal(t, toolchainHosts, SetupHosts(nil))
	assert.Equal(t, AgentHosts([]string{"go"}), AgentHosts([]string{"go", "go"}))
}

func TestHostsAreFresh(t *testing.T) {
	t.Parallel()

	h := SetupHosts(nil)
	h[0] = "changed"

	assert.NotEqual(t, "changed", SetupHosts(nil)[0])
}

// mise lists versions through its versions host and installs Rust with
// rustup's installer: toolchain hosts, so the agent phase never reaches them.
func TestToolchainHostsServeMiseAndRustup(t *testing.T) {
	t.Parallel()

	for _, h := range []string{"mise-versions.jdx.dev", "sh.rustup.rs"} {
		assert.Contains(t, SetupHosts(nil), h)
		assert.NotContains(t, AgentHosts(allEcosystems), h)
	}
}
