//go:build ecosystems

package runner

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

// ecosystemFixture is a tiny repository of one ecosystem with one real
// dependency its setup command fetches and one test, run by verify, that
// uses it.
type ecosystemFixture struct {
	name          string
	files         map[string]string
	setup, verify string
	tools         []string // as Prepare reports them
	// toolchainURL is on a host the toolchain comes from, which the agent
	// phase must not reach; version runs that toolchain there.
	toolchainURL, version string
	// added are files copied into /workspace in the agent phase; add installs
	// one more dependency there and runs code that uses it.
	added map[string]string
	add   string
	// checks must pass in the agent phase.
	checks []string
}

// hostileMise is a repository's mise.toml that must have no effect: mise
// never reads a configuration file under /workspace.
const hostileMise = `[env]
CAVU_FIXTURE_ENV = "set by the repository"

[tasks.marker]
run = "touch /workspace/mise-ran"

[hooks]
postinstall = "touch /workspace/mise-ran"
`

// noFixtureEnv fails when the repository's mise [env] reached the process.
const noFixtureEnv = `node -e 'process.exit(process.env.CAVU_FIXTURE_ENV === undefined ? 0 : 1)'`

var ecosystemFixtures = []ecosystemFixture{
	{
		name: "go",
		files: map[string]string{
			"go.mod": "module example.com/eco\n\ngo " + goVersion + "\n\nrequire github.com/google/uuid v1.6.0\n",
			"go.sum": "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\n" +
				"github.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=\n",
			"eco.go": "package eco\n\nimport \"github.com/google/uuid\"\n\n" +
				"func Valid(s string) bool {\n\t_, err := uuid.Parse(s)\n\n\treturn err == nil\n}\n",
			"eco_test.go": "package eco\n\nimport \"testing\"\n\n" +
				"func TestValid(t *testing.T) {\n\tif !Valid(\"6ba7b810-9dad-11d1-80b4-00c04fd430c8\") {\n\t\tt.Fatal(\"not valid\")\n\t}\n}\n",
		},
		setup: "go mod download", verify: "go test ./...",
		tools:        []string{"go@" + goVersion},
		toolchainURL: "https://go.dev/dl/", version: "go version",
		added: map[string]string{
			"added_test.go": "package eco\n\nimport (\n\t\"testing\"\n\n\t\"github.com/dustin/go-humanize\"\n)\n\n" +
				"func TestAdded(t *testing.T) {\n\tif got := humanize.Comma(1234567); got != \"1,234,567\" {\n\t\tt.Fatal(got)\n\t}\n}\n",
		},
		add: "go get github.com/dustin/go-humanize@v1.1.0 && go test -run TestAdded ./...",
	},
	{
		name: "node",
		files: map[string]string{
			"package.json": `{"name": "eco", "version": "1.0.0", "private": true, "scripts": {"test": "node --test"}, "dependencies": {"ms": "2.1.3"}}` + "\n",
			"package-lock.json": `{
  "name": "eco",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {"name": "eco", "version": "1.0.0", "dependencies": {"ms": "2.1.3"}},
    "node_modules/ms": {
      "version": "2.1.3",
      "resolved": "https://registry.npmjs.org/ms/-/ms-2.1.3.tgz",
      "integrity": "sha512-6FlzubTLZG3J2a/NVCAleEhjzq5oxgHyaCU9yYXvcLsvoVaHJq/s5xXI6/XXP6tz7R9xAOtHnSO/tXtF3WRTlA=="
    }
  }
}
`,
			"eco.test.js": "const test = require('node:test');\nconst assert = require('node:assert');\nconst ms = require('ms');\n\n" +
				"test('ms parses a duration', () => {\n  assert.strictEqual(ms('2s'), 2000);\n});\n",
			"mise.toml": hostileMise,
		},
		setup: "npm ci && " + noFixtureEnv, verify: "npm test",
		tools:        []string{"node@lts"},
		toolchainURL: "https://nodejs.org/dist/index.json", version: "node --version",
		add:    `npm install --no-audit --no-fund is-number@7.0.0 && node -e 'process.exit(require("is-number")(5) ? 0 : 1)'`,
		checks: []string{noFixtureEnv, "test ! -e /workspace/mise-ran"},
	},
	{
		name: "python",
		files: map[string]string{
			"requirements.txt": "six==1.17.0\npytest==9.2.0\n",
			"test_eco.py":      "import six\n\n\ndef test_string_types():\n    assert isinstance(\"x\", six.string_types)\n",
		},
		// pytest is a console script the setup command installs; pycodestyle
		// is one the agent installs for its user.
		setup: "pip install -r requirements.txt", verify: "pytest -q",
		tools:        []string{"python@3"},
		toolchainURL: "https://github.com/astral-sh/python-build-standalone/releases", version: "python --version",
		add: "pip install pycodestyle==2.15.0 && pycodestyle --version",
	},
	{
		name: "rust",
		files: map[string]string{
			"Cargo.toml": "[package]\nname = \"eco\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"=1.0.11\"\n",
			"src/lib.rs": "pub fn format(n: u64) -> String {\n    itoa::Buffer::new().format(n).to_string()\n}\n\n" +
				"#[cfg(test)]\nmod tests {\n    #[test]\n    fn formats() {\n        assert_eq!(super::format(42), \"42\");\n    }\n}\n",
		},
		// Offline, so the baseline and the second verify pass only on the
		// crates the setup phase fetched; the dependency add stays online.
		setup: "cargo fetch", verify: "cargo test --offline",
		tools:        []string{"rust@stable"},
		toolchainURL: "https://static.rust-lang.org/dist/channel-rust-stable.toml.sha256", version: "cargo --version",
		added: map[string]string{
			"examples/added.rs": "fn main() {\n    assert_eq!(ryu::Buffer::new().format(1.5), \"1.5\");\n}\n",
		},
		add: "cargo add ryu@=1.0.18 && cargo run --example added",
	},
}

// TestPrepareEcosystems prepares one repository of each ecosystem and uses
// it the way a session does: verify again, and add a dependency, with the
// project cache read-only and only the registries reachable.
func TestPrepareEcosystems(t *testing.T) {
	if os.Getenv("CAVU_TEST_OFFLINE") == "1" {
		t.Skip("CAVU_TEST_OFFLINE=1: this test downloads toolchains and packages through the allowlist")
	}

	for _, f := range ecosystemFixtures {
		t.Run(f.name, func(t *testing.T) { testEcosystem(t, f) })
	}
}

func testEcosystem(t *testing.T, f ecosystemFixture) {
	r, c, head := fixtureRun(t, f.files)
	c.Setup.Command = f.setup
	c.Verify.Command, c.Verify.Timeout = f.verify, 10*time.Minute

	started := time.Now()
	res, err := r.Prepare(t.Context(), c)
	cold := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, head, res.BaseCommit)
	assert.Equal(t, f.tools, res.Tools)
	assert.Equal(t, []string{f.name}, res.Ecosystems)
	assert.Equal(t, engine.ResultPassed, res.Baseline)

	st := r.state(c.RunID)
	require.NotNil(t, st.agent)

	out := runIn(t, st, "touch", "/cache/x")
	assert.NotEqual(t, 0, out.ExitCode, "the agent phase writes the project cache")

	vr, err := runVerify(t.Context(), st, c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)

	out = runIn(t, st, "curl", "--silent", "--fail", "--max-time", "30", "--output", "/dev/null", f.toolchainURL)
	assert.NotEqual(t, 0, out.ExitCode, "the agent phase reaches the toolchain host")

	out = runIn(t, st, "bash", "-c", f.version)
	assert.Equal(t, 0, out.ExitCode, out.Output)

	for _, check := range f.checks {
		out = runIn(t, st, "bash", "-c", check)
		assert.Equal(t, 0, out.ExitCode, "%s: %s", check, out.Output)
	}

	files := map[string][]byte{}
	for name, content := range f.added {
		files[workspace+"/"+name] = []byte(content)
	}

	require.NoError(t, st.agent.CopyIn(t.Context(), files))

	out = runIn(t, st, "bash", "-c", f.add)
	assert.Equal(t, 0, out.ExitCode, out.Output)

	started = time.Now()
	res, err = r.Prepare(t.Context(), c)
	warm := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, engine.ResultPassed, res.Baseline)

	t.Logf("%s %s: cold prepare %s, warm prepare %s", r.rt.Kind, f.name, cold.Round(time.Millisecond), warm.Round(time.Millisecond))
}
