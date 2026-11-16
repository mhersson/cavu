package env

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allEcosystems = []string{"go", "node", "python", "rust"}

func envMap(t *testing.T, entries []string) map[string]string {
	t.Helper()

	m := map[string]string{}

	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		require.True(t, ok, e)

		_, dup := m[k]
		require.False(t, dup, "duplicate key %s", k)
		m[k] = v
		keys = append(keys, k)
	}

	assert.IsIncreasing(t, keys)

	return m
}

func TestAgentEnvKeepsCacheReadOnlyPaths(t *testing.T) {
	t.Parallel()

	m := envMap(t, AgentEnv(allEcosystems, "/home/agent"))
	for _, k := range []string{"GOMODCACHE", "GOCACHE", "npm_config_cache", "PIP_CACHE_DIR", "UV_CACHE_DIR", "CARGO_HOME"} {
		assert.True(t, strings.HasPrefix(m[k], "/home/agent/"), "%s=%s", k, m[k])
		assert.False(t, strings.HasPrefix(m[k], "/cache"), "%s=%s", k, m[k])
	}

	assert.True(t, strings.HasPrefix(m["GOPROXY"], "file:///cache/go/mod/cache/download,"))
	assert.Equal(t, "-modcacherw", m["GOFLAGS"])
}

func TestSetupEnvCachesWritable(t *testing.T) {
	t.Parallel()
	m := envMap(t, SetupEnv(allEcosystems))
	assert.Equal(t, "/cache/go/mod", m["GOMODCACHE"])
	assert.Equal(t, "/cache/go/build", m["GOCACHE"])
	assert.Equal(t, "/cache/npm", m["npm_config_cache"])
	assert.Equal(t, "/cache/pip", m["PIP_CACHE_DIR"])
	assert.Equal(t, "/cache/uv", m["UV_CACHE_DIR"])
	assert.Equal(t, "/cache/cargo", m["CARGO_HOME"])
}

func TestEcosystemVariablesOnlyForGiven(t *testing.T) {
	t.Parallel()

	for _, e := range [][]string{SetupEnv([]string{"node"}), AgentEnv([]string{"node"}, "/home/agent")} {
		m := envMap(t, e)
		assert.Contains(t, m, "npm_config_cache")

		for _, k := range []string{"GOMODCACHE", "GOCACHE", "GOPROXY", "GOFLAGS", "PIP_CACHE_DIR", "UV_CACHE_DIR", "CARGO_HOME"} {
			assert.NotContains(t, m, k)
		}
	}
}

func TestTrustVariablesInBothPhases(t *testing.T) {
	t.Parallel()

	keys := []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "PIP_CERT", "CARGO_HTTP_CAINFO", "GIT_SSL_CAINFO"}

	for name, e := range map[string][]string{
		"setup": SetupEnv(nil), "agent": AgentEnv(nil, "/home/agent"),
		"setup all": SetupEnv(allEcosystems), "agent all": AgentEnv(allEcosystems, "/home/agent"),
	} {
		m := envMap(t, e)
		for _, k := range keys {
			assert.Equal(t, "/etc/ssl/certs/ca-certificates.crt", m[k], "%s %s", name, k)
		}
	}
}

func TestMiseIsolationInBothPhases(t *testing.T) {
	t.Parallel()

	for name, e := range map[string][]string{"setup": SetupEnv(nil), "agent": AgentEnv(nil, "/home/agent")} {
		m := envMap(t, e)
		v, ok := m["MISE_TRUSTED_CONFIG_PATHS"]
		assert.True(t, ok, name)
		assert.Empty(t, v, name)
		assert.Equal(t, "/workspace", m["MISE_IGNORED_CONFIG_PATHS"], name)
		assert.Equal(t, "/cache/mise", m["MISE_DATA_DIR"], name)
		assert.Equal(t, "/cache/mise/cavu.toml", m["MISE_GLOBAL_CONFIG_FILE"], name)
		assert.True(t, strings.HasPrefix(m["PATH"], "/cache/mise/shims:"), name)
	}
}

func TestRustupHomeIsSharedInBothPhases(t *testing.T) {
	t.Parallel()

	for name, e := range map[string][]string{
		"setup": SetupEnv([]string{"rust"}), "agent": AgentEnv([]string{"rust"}, "/home/agent"),
	} {
		assert.Equal(t, "/cache/rustup", envMap(t, e)["RUSTUP_HOME"], name)
	}

	for name, e := range map[string][]string{
		"setup": SetupEnv([]string{"go"}), "agent": AgentEnv([]string{"go"}, "/home/agent"),
	} {
		assert.NotContains(t, envMap(t, e), "RUSTUP_HOME", name)
	}
}

// The setup phase reaches mise's versions host, which must not learn what a
// project downloads.
func TestMiseReportsNoDownloads(t *testing.T) {
	t.Parallel()

	for name, e := range map[string][]string{"setup": SetupEnv(nil), "agent": AgentEnv(nil, "/home/agent")} {
		assert.Equal(t, "false", envMap(t, e)["MISE_USE_VERSIONS_HOST_TRACK"], name)
	}
}

// mise finds Rust's binaries under CARGO_HOME and hands every command that
// CARGO_HOME, so the agent's writable one links to the binaries the setup
// phase installed in the cache.
func TestAgentLinksCargoBinaries(t *testing.T) {
	t.Parallel()

	agent := envMap(t, AgentEnv([]string{"rust"}, "/home/agent"))
	setup := envMap(t, SetupEnv([]string{"rust"}))

	assert.Equal(t, map[string]string{agent["CARGO_HOME"] + "/bin": setup["CARGO_HOME"] + "/bin"},
		AgentLinks([]string{"rust"}, "/home/agent"))

	for _, ecosystems := range [][]string{nil, {"go", "node", "python"}} {
		assert.Empty(t, AgentLinks(ecosystems, "/home/agent"), ecosystems)
	}
}

// A tool missing in the agent phase stays missing: an install there would
// write under the read-only cache and record a failed install in the home.
func TestMiseInstallsNothingInTheAgentPhase(t *testing.T) {
	t.Parallel()

	for _, ecosystems := range [][]string{nil, allEcosystems} {
		assert.Equal(t, "false", envMap(t, AgentEnv(ecosystems, "/home/agent"))["MISE_AUTO_INSTALL"], ecosystems)
	}
}
