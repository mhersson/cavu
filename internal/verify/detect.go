// Package verify holds the pure parts of the verify gate: suggesting a command
// from a repository's root files, wrapping it for bash, classifying the result,
// clamping the timeout and cutting a failure excerpt. It runs nothing and reads
// no disk.
package verify

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// npmInitPlaceholder is the scripts.test line `npm init` writes; it is not a
// real test command.
const npmInitPlaceholder = `echo "Error: no test specified" && exit 1`

// dotnetPatterns are the root file patterns that mark a .NET project.
var dotnetPatterns = []string{"*.sln", "*.csproj", "*.fsproj"}

// justfileTestRe matches a `test` recipe at column 0. The trailing guard keeps
// a variable assignment (`test := "x"`) from reading as a recipe colon.
var justfileTestRe = regexp.MustCompile(`^test([ \t][^:]*)?:(?:[^=]|$)`)

// SuggestFiles are the root files Suggest needs. Entries matching a glob
// (*.sln, *.csproj, *.fsproj) are patterns for path.Match: a caller lists the
// root and passes every entry that matches a pattern.
func SuggestFiles() []string {
	return append([]string{
		"Makefile", "justfile", "Justfile", ".justfile", "Taskfile.yml", "Taskfile.yaml",
		"go.mod", "Cargo.toml", "package.json",
		"pytest.ini", "pyproject.toml", "setup.cfg",
		"gradlew", "build.gradle", "build.gradle.kts",
		"mvnw", "pom.xml",
	}, dotnetPatterns...)
}

// Suggest returns the verify command a repository most likely wants, from the
// names and contents of its root files. ok is false when nothing fits.
// gradlew and mvnw count as present by name, as executable bits are not known.
func Suggest(files map[string][]byte) (command string, ok bool) {
	switch {
	case makefileHasTestTarget(files["Makefile"]):
		return "make test", true
	case justfileHasTestRecipe(files):
		return "just test", true
	case taskfileHasTestTask(files):
		return "task test", true
	}

	switch {
	case has(files, "go.mod"):
		return "go test ./...", true
	case has(files, "Cargo.toml"):
		return "cargo test", true
	case hasRealNPMTestScript(files["package.json"]):
		return "npm test", true
	case hasPytestMarker(files):
		return "pytest -q", true
	case has(files, "gradlew"):
		return "./gradlew test", true
	case has(files, "build.gradle"), has(files, "build.gradle.kts"):
		return "gradle test", true
	case has(files, "mvnw"):
		return "./mvnw -q test", true
	case has(files, "pom.xml"):
		return "mvn -q test", true
	case hasDotnetProject(files):
		return "dotnet test", true
	}

	return "", false
}

func has(files map[string][]byte, name string) bool {
	_, ok := files[name]

	return ok
}

// makefileHasTestTarget matches at column 0 only: indented lines are recipes.
func makefileHasTestTarget(data []byte) bool {
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "test:") {
			return true
		}
	}

	return false
}

func justfileHasTestRecipe(files map[string][]byte) bool {
	for _, name := range []string{"justfile", "Justfile", ".justfile"} {
		for line := range strings.SplitSeq(string(files[name]), "\n") {
			if justfileTestRe.MatchString(line) {
				return true
			}
		}
	}

	return false
}

// taskfileHasTestTask scans lines rather than parsing YAML, so flow-style
// Taskfiles are not recognised. That only costs a suggestion.
func taskfileHasTestTask(files map[string][]byte) bool {
	for _, name := range []string{"Taskfile.yml", "Taskfile.yaml"} {
		if taskfileDeclaresTest(string(files[name])) {
			return true
		}
	}

	return false
}

func taskfileDeclaresTest(data string) bool {
	inTasks := false
	childIndent := -1

	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimRight(line, " \t\r")
		body := strings.TrimLeft(line, " \t")

		if body == "" || strings.HasPrefix(body, "#") {
			continue
		}

		indent := len(line) - len(body)

		if indent == 0 {
			rest, ok := strings.CutPrefix(body, "tasks:")
			rest = strings.TrimSpace(rest)
			inTasks = ok && (rest == "" || strings.HasPrefix(rest, "#"))
			childIndent = -1

			continue
		}

		if !inTasks {
			continue
		}

		if childIndent < 0 {
			childIndent = indent
		}

		if indent == childIndent && isTestKey(body) {
			return true
		}
	}

	return false
}

func isTestKey(body string) bool {
	for _, key := range []string{"test", `"test"`, `'test'`} {
		rest, ok := strings.CutPrefix(body, key+":")
		if !ok {
			continue
		}

		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return true
		}
	}

	return false
}

func hasRealNPMTestScript(data []byte) bool {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}

	if json.Unmarshal(data, &pkg) != nil {
		return false
	}

	test := strings.TrimSpace(pkg.Scripts["test"])

	return test != "" && test != npmInitPlaceholder
}

// hasPytestMarker needs explicit pytest configuration: a bare pyproject.toml
// would run pytest on a project that may have no tests, which exits 5.
func hasPytestMarker(files map[string][]byte) bool {
	return has(files, "pytest.ini") ||
		strings.Contains(string(files["pyproject.toml"]), "[tool.pytest") ||
		strings.Contains(string(files["setup.cfg"]), "[tool:pytest]")
}

func hasDotnetProject(files map[string][]byte) bool {
	for name := range files {
		for _, pat := range dotnetPatterns {
			if m, _ := path.Match(pat, name); m {
				return true
			}
		}
	}

	return false
}
