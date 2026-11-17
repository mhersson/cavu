package env

import (
	"path"
	"slices"
	"sort"
)

// Container paths; they must match where the sandbox mounts its volumes and
// where the image keeps the trust bundle.
const (
	cacheDir     = "/cache"
	workspaceDir = "/workspace"
	caBundle     = "/etc/ssl/certs/ca-certificates.crt"
)

const defaultPath = cacheDir + "/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// common is set in every phase. Trust variables make tools that ignore the
// system store still trust the instance's CA. Pointing mise at an ignored
// /workspace keeps a repository's own mise file from ever being executed.
// mise tells its versions host, which the setup phase reaches for version
// lists, nothing about what a project downloads.
func common() map[string]string {
	return map[string]string{
		"SSL_CERT_FILE":                caBundle,
		"NODE_EXTRA_CA_CERTS":          caBundle,
		"REQUESTS_CA_BUNDLE":           caBundle,
		"PIP_CERT":                     caBundle,
		"CARGO_HTTP_CAINFO":            caBundle,
		"GIT_SSL_CAINFO":               caBundle,
		"MISE_DATA_DIR":                cacheDir + "/mise",
		"MISE_GLOBAL_CONFIG_FILE":      MiseConfigFile,
		"MISE_TRUSTED_CONFIG_PATHS":    "",
		"MISE_IGNORED_CONFIG_PATHS":    workspaceDir,
		"MISE_USE_VERSIONS_HOST_TRACK": "false",
		"PATH":                         defaultPath,
	}
}

// cacheVars sets each ecosystem's variables, its caches under root.
func cacheVars(m map[string]string, ecosystems []string, root string) {
	for _, e := range ecosystems {
		switch e {
		case "go":
			m["GOMODCACHE"] = path.Join(root, "go/mod")
			m["GOCACHE"] = path.Join(root, "go/build")
		case "node":
			m["npm_config_cache"] = path.Join(root, "npm")
		case "python":
			m["PIP_CACHE_DIR"] = path.Join(root, "pip")
			m["UV_CACHE_DIR"] = path.Join(root, "uv")
			// uv runs on mise's interpreter, and copies: its cache is never on the workspace's volume.
			m["UV_PYTHON_DOWNLOADS"] = "never"
			m["UV_LINK_MODE"] = "copy"
		case "rust":
			m["CARGO_HOME"] = cargoHome(root)
			m["RUSTUP_HOME"] = cacheDir + "/rustup"
		}
	}
}

func cargoHome(root string) string { return path.Join(root, "cargo") }

// agentRoot holds the agent phase's caches, in its writable home.
func agentRoot(home string) string { return path.Join(home, ".cache") }

func render(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

// The mise toolchains come read-only from the project cache, and mise installs
// nothing: the toolchain hosts are out of reach.
//
//   - Python: the packages the setup command installed are read-only in the
//     cache's interpreter and shared by every task of the project. They pile
//     up across prepares, so a requirement the project drops stays importable;
//     a project that wants its own makes a virtual environment in the setup
//     command, as uv sync does, in a directory git ignores so no commit
//     takes it.
//   - Rust: Cargo's binaries come through AgentLinks and its downloads through
//     AgentSeeds.
func AgentEnv(ecosystems []string, home string) []string {
	m := common()
	cacheVars(m, ecosystems, agentRoot(home))
	m["MISE_AUTO_INSTALL"] = "false"
	// Last, so a script the agent installs for its user shadows no tool.
	m["PATH"] = defaultPath + ":" + path.Join(home, ".local/bin")

	for _, e := range ecosystems {
		if e == "go" {
			m["GOPROXY"] = "file://" + cacheDir + "/go/mod/cache/download,https://proxy.golang.org,direct"
			m["GOFLAGS"] = "-modcacherw"
		}
	}

	return render(m)
}

// AgentLinks are the symbolic links, path to target, that the agent phase
// needs in home each time it comes up. mise looks for Rust's binaries under
// CARGO_HOME and hands every command that CARGO_HOME, so the agent's writable
// Cargo home links its bin to the binaries the setup phase installed, which
// are read-only: the agent's cargo install cannot write there.
func AgentLinks(ecosystems []string, home string) map[string]string {
	if !slices.Contains(ecosystems, "rust") {
		return nil
	}

	return map[string]string{
		path.Join(cargoHome(agentRoot(home)), "bin"): path.Join(cargoHome(cacheDir), "bin"),
	}
}

// AgentSeeds are the directories, path to source, that the agent phase needs
// filled in home from the read-only cache each time it comes up: Cargo's
// registry index, crate archives and git databases. A build there then needs
// neither the network nor a credential only the setup phase had, and a crate
// the agent adds still lands in its writable Cargo home.
func AgentSeeds(ecosystems []string, home string) map[string]string {
	if !slices.Contains(ecosystems, "rust") {
		return nil
	}

	seeds := map[string]string{}
	for _, dir := range []string{"registry/index", "registry/cache", "git/db"} {
		seeds[path.Join(cargoHome(agentRoot(home)), dir)] = path.Join(cargoHome(cacheDir), dir)
	}

	return seeds
}

// SetupEnv returns the environment of the setup phase: caches under /cache, writable.
func SetupEnv(ecosystems []string) []string {
	m := common()
	cacheVars(m, ecosystems, cacheDir)

	return render(m)
}
