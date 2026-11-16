// Package env turns a repository's root version files into the tools to install
// with mise, the mise configuration CAVU writes, the egress hosts of the setup
// and agent phases, and each phase's cache and trust environment. It is pure:
// it reads no disk, starts no process and looks up no environment variable.
package env

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Tool is one mise tool and the version to install.
type Tool struct{ Name, Version string }

// Defaults for an ecosystem present with no version source. They are floating:
// they move with upstream, and an override pins them.
const (
	defaultNode   = "lts"
	defaultRust   = "stable"
	defaultPython = "3"
)

var (
	goVersionRe    = regexp.MustCompile(`^\d+(\.\d+){0,2}([a-z]+\d+)?$`)
	exactVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	majorRe        = regexp.MustCompile(`^\^?(\d+)(\.x)?$`)
	pythonMinRe    = regexp.MustCompile(`^>=\s*(\d+\.\d+)$`)
)

// toolAliases maps the asdf names to mise's.
var toolAliases = map[string]string{"golang": "go", "nodejs": "node"}

// VersionFiles are the root-level files Detect reads from a clone.
func VersionFiles() []string {
	return []string{
		".nvmrc", ".python-version", ".tool-versions", "Cargo.toml", "go.mod",
		"mise.toml", "package.json", "pyproject.toml", "requirements.txt", "rust-toolchain.toml",
	}
}

// Detection is the result of Detect.
type Detection struct {
	Tools      []Tool
	Ecosystems []string
	Ignored    []string // repository entries that were not read, as written, for a person to see
}

// Detect computes the tool list and the ecosystems present. overrides replace
// or add versions. A file that cannot be parsed is an error naming the file.
// For one tool the precedence is: override, .tool-versions, mise.toml [tools],
// the tool's own file, then the floating default.
// From repository files it reads only plain registry names (no ":") and
// versions without a "path:" or "ref:" prefix: a backend or a path named by
// the repository could run code in the setup container. Such entries go to
// Ignored and shadow nothing. Overrides are the operator's and are exempt.
func Detect(files map[string][]byte, overrides map[string]string) (Detection, error) {
	for name, version := range overrides {
		if name == "" || version == "" {
			return Detection{}, errors.New("override needs a tool name and a version")
		}
	}

	r := &reader{}

	files = stripBOMs(files)

	own, err := r.ownVersions(files)
	if err != nil {
		return Detection{}, err
	}

	mise, err := r.parseMise(files["mise.toml"])
	if err != nil {
		return Detection{}, err
	}

	asdf, err := r.parseToolVersions(files[".tool-versions"])
	if err != nil {
		return Detection{}, err
	}

	ecosystems := detectEcosystems(files)
	versions := map[string]string{}

	for _, layer := range []map[string]string{own, mise, asdf, overrides} {
		maps.Copy(versions, layer)
	}

	for _, d := range []struct{ ecosystem, tool, version string }{
		{"node", "node", defaultNode},
		{"rust", "rust", defaultRust},
		{"python", "python", defaultPython},
	} {
		if _, ok := versions[d.tool]; !ok && slices.Contains(ecosystems, d.ecosystem) {
			versions[d.tool] = d.version
		}
	}

	var tools []Tool

	for name, version := range versions {
		tools = append(tools, Tool{Name: name, Version: version})
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	slices.Sort(r.ignored)

	return Detection{Tools: tools, Ecosystems: ecosystems, Ignored: slices.Compact(r.ignored)}, nil
}

// stripBOMs returns files without a leading UTF-8 byte order mark on any of them.
func stripBOMs(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, b := range files {
		out[name] = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	}

	return out
}

// reader collects the entries it declines to read.
type reader struct{ ignored []string }

// take stores a repository's version for a tool unless it names a backend or a
// path, which is recorded as ignored instead.
func (r *reader) take(out map[string]string, file, name, version string) error {
	if !utf8.ValidString(name) || !utf8.ValidString(version) {
		return fmt.Errorf("%s: a tool name or version is not valid UTF-8", file)
	}

	if strings.Contains(name, ":") || strings.HasPrefix(version, "path:") || strings.HasPrefix(version, "ref:") {
		r.ignored = append(r.ignored, file+": "+name+" "+version)

		return nil
	}

	out[normaliseTool(name)] = version

	return nil
}

func detectEcosystems(files map[string][]byte) []string {
	var out []string

	has := func(names ...string) bool {
		for _, n := range names {
			if _, ok := files[n]; ok {
				return true
			}
		}

		return false
	}
	if has("go.mod") {
		out = append(out, "go")
	}

	if has("package.json") {
		out = append(out, "node")
	}

	if has("pyproject.toml", "requirements.txt", ".python-version") {
		out = append(out, "python")
	}

	if has("Cargo.toml") {
		out = append(out, "rust")
	}

	return out
}

// ownVersions reads the versions each tool's own file declares.
func (r *reader) ownVersions(files map[string][]byte) (map[string]string, error) {
	out := map[string]string{}

	if b, ok := files["go.mod"]; ok {
		v, err := parseGoMod(b)
		if err != nil {
			return nil, err
		}

		if v != "" {
			out["go"] = v
		}
	}

	if b, ok := files["package.json"]; ok {
		node, manager, managerVersion, err := parsePackageJSON(b)
		if err != nil {
			return nil, err
		}

		if node != "" {
			out["node"] = node
		}

		if manager != "" {
			if err := r.take(out, "package.json", manager, managerVersion); err != nil {
				return nil, err
			}
		}
	}

	if b, ok := files[".nvmrc"]; ok {
		if v := firstLine(string(b), false); v != "" {
			v = strings.TrimPrefix(v, "v")
			if v == "lts/*" {
				v = "lts"
			}

			if err := r.take(out, ".nvmrc", "node", v); err != nil {
				return nil, err
			}
		}
	}

	if b, ok := files["pyproject.toml"]; ok {
		var doc struct {
			Project struct {
				RequiresPython string `toml:"requires-python"`
			} `toml:"project"`
		}
		if _, err := toml.Decode(string(b), &doc); err != nil {
			return nil, fmt.Errorf("pyproject.toml: %w", err)
		}

		if m := pythonMinRe.FindStringSubmatch(strings.TrimSpace(doc.Project.RequiresPython)); m != nil {
			out["python"] = m[1]
		}
	}

	if b, ok := files[".python-version"]; ok {
		if v := firstLine(string(b), true); v != "" {
			if err := r.take(out, ".python-version", "python", v); err != nil {
				return nil, err
			}
		}
	}

	if b, ok := files["rust-toolchain.toml"]; ok {
		var doc struct {
			Toolchain struct {
				Channel string `toml:"channel"`
			} `toml:"toolchain"`
		}
		if _, err := toml.Decode(string(b), &doc); err != nil {
			return nil, fmt.Errorf("rust-toolchain.toml: %w", err)
		}

		if doc.Toolchain.Channel != "" {
			if err := r.take(out, "rust-toolchain.toml", "rust", doc.Toolchain.Channel); err != nil {
				return nil, err
			}
		}
	}

	return out, nil
}

// firstLine returns the first non-blank line, trimmed; with skipComments it
// also passes over lines starting with #.
func firstLine(s string, skipComments bool) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (skipComments && strings.HasPrefix(line, "#")) {
			continue
		}

		return line
	}

	return ""
}

func parseGoMod(b []byte) (string, error) {
	var goVer, toolchain string

	for line := range strings.SplitSeq(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}

		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}

		switch f[0] {
		case "go":
			goVer = f[1]
		case "toolchain":
			if f[1] != "default" {
				toolchain = strings.TrimPrefix(f[1], "go")
			}
		}
	}

	for _, v := range []string{goVer, toolchain} {
		if v != "" && !goVersionRe.MatchString(v) {
			return "", fmt.Errorf("go.mod: %q is not a Go version", v)
		}
	}

	if toolchain != "" {
		return toolchain, nil
	}

	return goVer, nil
}

// parsePackageJSON returns no node version when engines.node is absent or not
// an accepted form; the defaults table then applies.
func parsePackageJSON(b []byte) (node, manager, managerVersion string, err error) {
	var doc struct {
		Engines        struct{ Node string } `json:"engines"`
		PackageManager string                `json:"packageManager"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", "", "", fmt.Errorf("package.json: %w", err)
	}

	engine := strings.TrimSpace(doc.Engines.Node)
	if exactVersionRe.MatchString(engine) {
		node = engine
	} else if m := majorRe.FindStringSubmatch(engine); m != nil {
		node = m[1]
	}

	name, version, _ := strings.Cut(doc.PackageManager, "@")
	if name == "pnpm" || name == "yarn" {
		version, _, _ = strings.Cut(version, "+")
		if version != "" {
			return node, name, version, nil
		}
	}

	return node, "", "", nil
}

func normaliseTool(name string) string {
	if a, ok := toolAliases[name]; ok {
		return a
	}

	return name
}

func (r *reader) parseToolVersions(b []byte) (map[string]string, error) {
	out := map[string]string{}

	for line := range strings.SplitSeq(string(b), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}

		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}

		if len(f) < 2 {
			return nil, fmt.Errorf(".tool-versions: %q has no version", f[0])
		}

		if err := r.take(out, ".tool-versions", f[0], f[1]); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (r *reader) parseMise(b []byte) (map[string]string, error) {
	if b == nil {
		return nil, nil
	}

	var doc struct {
		Tools map[string]any `toml:"tools"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil, fmt.Errorf("mise.toml: %w", err)
	}

	out := map[string]string{}

	for name, raw := range doc.Tools {
		var version string

		switch v := raw.(type) {
		case string:
			version = v
		case []any:
			if len(v) > 0 {
				version, _ = v[0].(string)
			}
		case map[string]any:
			version, _ = v["version"].(string)
		}

		if version == "" {
			return nil, fmt.Errorf("mise.toml: tool %q has no version string", name)
		}

		if err := r.take(out, "mise.toml", name, version); err != nil {
			return nil, err
		}
	}

	return out, nil
}
