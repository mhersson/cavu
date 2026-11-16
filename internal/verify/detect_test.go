package verify

import (
	"encoding/json"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func files(pairs ...string) map[string][]byte {
	m := map[string][]byte{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = []byte(pairs[i+1])
	}

	return m
}

func TestSuggest(t *testing.T) {
	tests := []struct {
		name  string
		files map[string][]byte
		want  string
		ok    bool
	}{
		{"makefile beats go.mod", files("Makefile", "build:\n\tgo build\ntest:\n\tgo test\n", "go.mod", "module x"), "make test", true},
		{"makefile target with deps", files("Makefile", "test: build\n\tx\n"), "make test", true},
		{"indented test is no target", files("Makefile", "build:\n\ttest: x\n  test:\n"), "", false},
		{"justfile recipe", files("justfile", "test:\n  go test\n"), "just test", true},
		{"Justfile with params", files("Justfile", "test *args:\n  go test\n"), "just test", true},
		{".justfile recipe", files(".justfile", "test:\n  x\n"), "just test", true},
		{"justfile variable is no recipe", files("justfile", "test := \"x\"\n"), "", false},
		{"taskfile task", files("Taskfile.yml", "version: '3'\ntasks:\n  test:\n    cmds: [x]\n"), "task test", true},
		{"taskfile yaml quoted", files("Taskfile.yaml", "tasks:\n    \"test\":\n      cmds: [x]\n"), "task test", true},
		{"taskfile test with comment", files("Taskfile.yml", "tasks:\n  # c\n  test: # run\n    cmds: [x]\n"), "task test", true},
		{"taskfile other task", files("Taskfile.yml", "tasks:\n  build:\n    cmds: [x]\n  testing:\n    cmds: [x]\n"), "", false},
		{"taskfile nested test is no task", files("Taskfile.yml", "tasks:\n  build:\n    test:\n      x: 1\n"), "", false},
		{"taskfile test outside tasks", files("Taskfile.yml", "vars:\n  test: x\ntasks:\n  build:\n    cmds: [x]\n"), "", false},
		{"go.mod", files("go.mod", "module x"), "go test ./...", true},
		{"cargo", files("Cargo.toml", ""), "cargo test", true},
		{"go before cargo", files("go.mod", "", "Cargo.toml", ""), "go test ./...", true},
		{"npm real script", files("package.json", `{"scripts":{"test":"vitest"}}`), "npm test", true},
		{"npm placeholder", files("package.json", `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`), "", false},
		{"npm no scripts", files("package.json", `{}`), "", false},
		{"npm invalid json", files("package.json", `{`), "", false},
		{"pytest.ini", files("pytest.ini", ""), "pytest -q", true},
		{"pyproject pytest table", files("pyproject.toml", "[tool.pytest.ini_options]\n"), "pytest -q", true},
		{"setup.cfg pytest", files("setup.cfg", "[tool:pytest]\n"), "pytest -q", true},
		{"bare pyproject", files("pyproject.toml", "[project]\nname='x'\n"), "", false},
		{"gradlew", files("gradlew", "", "build.gradle", ""), "./gradlew test", true},
		{"gradle build file", files("build.gradle.kts", ""), "gradle test", true},
		{"mvnw", files("mvnw", "", "pom.xml", ""), "./mvnw -q test", true},
		{"pom", files("pom.xml", ""), "mvn -q test", true},
		{"csproj", files("App.csproj", ""), "dotnet test", true},
		{"sln", files("App.sln", ""), "dotnet test", true},
		{"fsproj", files("App.fsproj", ""), "dotnet test", true},
		{"nothing", files("README.md", "hi"), "", false},
		{"empty", nil, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Suggest(tt.files)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSuggestFilesCoverEveryConsultedKey(t *testing.T) {
	fixtures := []map[string][]byte{
		files("Makefile", "test:\n"), files("justfile", "test:\n"), files("Justfile", "test:\n"), files(".justfile", "test:\n"),
		files("Taskfile.yml", "tasks:\n  test:\n"), files("Taskfile.yaml", "tasks:\n  test:\n"),
		files("go.mod", ""), files("Cargo.toml", ""), files("package.json", `{"scripts":{"test":"x"}}`),
		files("pytest.ini", ""), files("pyproject.toml", "[tool.pytest"), files("setup.cfg", "[tool:pytest]"),
		files("gradlew", ""), files("build.gradle", ""), files("build.gradle.kts", ""),
		files("mvnw", ""), files("pom.xml", ""),
		files("a.sln", ""), files("a.csproj", ""), files("a.fsproj", ""),
	}

	entries := SuggestFiles()

	for _, fx := range fixtures {
		_, ok := Suggest(fx)
		require.True(t, ok)

		for name := range fx {
			matched := false

			for _, e := range entries {
				if m, _ := path.Match(e, name); m {
					matched = true
				}
			}

			assert.True(t, matched, name)
		}
	}

	entries[0] = "changed"

	assert.NotEqual(t, "changed", SuggestFiles()[0])
}

func TestNpmPlaceholderIsNotATestScript(t *testing.T) {
	b, err := json.Marshal(map[string]any{"scripts": map[string]string{"test": npmInitPlaceholder}})
	require.NoError(t, err)

	_, ok := Suggest(map[string][]byte{"package.json": b})
	assert.False(t, ok)
}
