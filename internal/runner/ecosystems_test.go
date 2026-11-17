//go:build ecosystems

package runner

import (
	"cmp"
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
	ecosystem     string // as Prepare reports it, when it is not name
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
			"mise.toml":  hostileMise,
			".gitignore": "node_modules/\n",
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
			".gitignore":       "__pycache__/\n.pytest_cache/\n",
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
		name: "python-uv", ecosystem: "python",
		files: map[string]string{
			".python-version": "3.12\n",
			".gitignore":      ".venv/\n__pycache__/\n.pytest_cache/\n",
			"pyproject.toml": `[project]
name = "eco"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["six==1.17.0"]

[dependency-groups]
dev = ["pytest==9.2.0"]

[build-system]
requires = ["uv_build>=0.13.8,<0.14.0"]
build-backend = "uv_build"
`,
			"uv.lock": `version = 1
revision = 5
requires-python = ">=3.12"

[[package]]
name = "colorama"
version = "0.4.6"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/d8/53/6f443c9a4a8358a93a6792e2acffb9d9d5cb0a5cfd8802644b7b1c9a02e4/colorama-0.4.6.tar.gz", hash = "sha256:08695f5cb7ed6e0531a20572697297273c47b8cae5a63ffc6d6ed5c201be6e44", size = 27697, upload-time = "2022-10-25T02:36:22.414Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/d1/d6/3965ed04c63042e047cb6a3e6ed1a63a35087b6a609aa3a15ed8ac56c221/colorama-0.4.6-py2.py3-none-any.whl", hash = "sha256:4f1d9991f5acc0ca119f9d443620b77f9d6b33703e51011c16baf57afb285fc6", size = 25335, upload-time = "2022-10-25T02:36:20.889Z" },
]

[[package]]
name = "eco"
version = "0.1.0"
source = { editable = "." }
dependencies = [
    { name = "six" },
]

[package.dev-dependencies]
dev = [
    { name = "pytest" },
]

[package.metadata]
requires-dist = [{ name = "six", specifier = "==1.17.0" }]

[package.metadata.requires-dev]
dev = [{ name = "pytest", specifier = "==9.2.0" }]

[[package]]
name = "iniconfig"
version = "2.3.1"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/01/e1/2069291243c926a2ff1cd706c7f3eeb9b62144bf60f77c9fb9ff2fb26bd3/iniconfig-2.3.1.tar.gz", hash = "sha256:67f4b9c50da0dedf52af349e7749a80a9057a5031199791b906c3bb3ae878960", size = 21209, upload-time = "2026-10-06T22:48:38.076Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/56/43/4ca9e49d27a1fcf6bece6f6aec0ea46bb9112489b93d4b688fb415457bdb/iniconfig-2.3.1-py3-none-any.whl", hash = "sha256:9121e2c1fdb355232495be3194c8dfe87ccc2d5dee45947b78e68f499790d7a7", size = 7552, upload-time = "2026-10-06T22:48:36.959Z" },
]

[[package]]
name = "packaging"
version = "26.3"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/7d/fa/3944b40b07da9ce895c0e6303a5ab7d53da063554f534556b134a54d6093/packaging-26.3.tar.gz", hash = "sha256:94edc256424af38762eb31306eed28beb9f0efc50a8837492c9d6fd6004aed79", size = 313412, upload-time = "2026-08-04T18:15:28.737Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/63/34/ba1c580383c9eada3711951fef0795c80b829a078d72188184bcab9dd527/packaging-26.3-py3-none-any.whl", hash = "sha256:d7193f7c8e4e93f444fde0262bf90af30e16fa0ad0ad44cb553c87339b23cd1c", size = 129956, upload-time = "2026-08-04T18:15:27.159Z" },
]

[[package]]
name = "pluggy"
version = "1.7.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/bf/db/7fc19e6f2dc92a966727031389fc2e08b558f0f25eb7403c1119ad4713cd/pluggy-1.7.0.tar.gz", hash = "sha256:d1eaa46ebb595891b860ab086b4d09c8588af65ebd4361b8e8f4bb8920b90ba8", size = 123304, upload-time = "2026-10-15T09:50:58.343Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/40/9e/2b38731e0fc536806f16490e1a12d7f0dc2a1235aa8cc07bcc75416a7daa/pluggy-1.7.0-py3-none-any.whl", hash = "sha256:7dd7b0d8832ba3cb632c306926ded123429211b83641b35dc5c41ad2d34f9bec", size = 27082, upload-time = "2026-10-15T09:50:56.808Z" },
]

[[package]]
name = "pygments"
version = "2.21.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/49/2e/ced460408999b33da6b31b0021b0f37d329e202d4169aeb164493778f25b/pygments-2.21.0.tar.gz", hash = "sha256:610ca751c9bc2492b38eb9a38a7fbc93edbbb2d7182edaf34e66ae493dee5c8c", size = 5005329, upload-time = "2026-08-17T08:02:48.824Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/71/46/17f022dd3e953bf20a04a028a21ec746d942f8d2af30fa0f124fa0e6a684/pygments-2.21.0-py3-none-any.whl", hash = "sha256:2363c69b61c4a97c838da3b130dcd6468f4848992b21a82f2a63ec34377137d9", size = 1250147, upload-time = "2026-08-17T08:02:44.912Z" },
]

[[package]]
name = "pytest"
version = "9.2.0"
source = { registry = "https://pypi.org/simple" }
dependencies = [
    { name = "colorama", marker = "sys_platform == 'win32'" },
    { name = "iniconfig" },
    { name = "packaging" },
    { name = "pluggy" },
    { name = "pygments" },
]
sdist = { url = "https://files.pythonhosted.org/packages/79/42/bb4641652e58dedeac2ec51abc1f8cae4fee4ec63899a0999016229a0f2a/pytest-9.2.0.tar.gz", hash = "sha256:97c4f3b1f3c7414aa36465e09b8220f1d03627993bd51b1282e193753c80557b", size = 1797555, upload-time = "2026-10-27T21:30:20.728Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/51/39/1f5d8c8877a0fa327db959c19341d4be61660de1ab6b7391f9eb805bcd67/pytest-9.2.0-py3-none-any.whl", hash = "sha256:07acdcf88ec174f907dc01af70bf52d49b014bbab03faeeade31c96b18c2e588", size = 411776, upload-time = "2026-10-27T21:30:19.131Z" },
]

[[package]]
name = "six"
version = "1.17.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/94/e7/b2c673351809dca68a0e064b6af791aa332cf192da575fd474ed7d6f16a2/six-1.17.0.tar.gz", hash = "sha256:ff70335d468e7eb6ec65b95b99d3a2836546063f63acc5171de367e834932a81", size = 34031, upload-time = "2024-12-04T17:35:28.174Z" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/b7/ce/149a00dd41f10bc29e5921b496af8b574d8413afcd5e30dfa0ed46c2cc5e/six-1.17.0-py2.py3-none-any.whl", hash = "sha256:4721f391ed90541fddacab5acf947aa0d3dc7d27b2e1e8eda2be8970586c3274", size = 11050, upload-time = "2024-12-04T17:35:26.475Z" },
]
`,
			"src/eco/__init__.py": "import six\n\n\ndef is_text(value):\n    return isinstance(value, six.string_types)\n",
			"tests/test_eco.py":   "from eco import is_text\n\n\ndef test_is_text():\n    assert is_text(\"x\")\n",
		},
		setup: "uv sync --frozen", verify: "uv run pytest -q",
		tools:        []string{"python@3.12", "uv@latest"},
		toolchainURL: "https://github.com/astral-sh/python-build-standalone/releases", version: "uv --version",
		add:    "uv add pycodestyle==2.15.0 && uv run pycodestyle --version",
		checks: []string{`uv run python -c 'import sys; assert sys.base_prefix.startswith("/cache/mise/"), sys.base_prefix'`},
	},
	{
		name: "rust",
		files: map[string]string{
			".gitignore": "target/\nCargo.lock\n",
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

// TestPrepareEcosystems prepares one repository per fixture and uses
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
	assert.Equal(t, []string{cmp.Or(f.ecosystem, f.name)}, res.Ecosystems)
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
