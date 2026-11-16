package env

import (
	"path"
	"sort"
)

// Container paths shared with the sandbox layout.
const (
	cacheDir     = "/cache"
	workspaceDir = "/workspace"
	caBundle     = "/etc/ssl/certs/ca-certificates.crt"
)

const defaultPath = cacheDir + "/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// common is set in both phases. Trust variables make tools that ignore the
// system store still trust the instance's CA. Pointing mise at an ignored
// /workspace keeps a repository's own mise file from ever being executed.
func common() map[string]string {
	return map[string]string{
		"SSL_CERT_FILE":             caBundle,
		"NODE_EXTRA_CA_CERTS":       caBundle,
		"REQUESTS_CA_BUNDLE":        caBundle,
		"PIP_CERT":                  caBundle,
		"CARGO_HTTP_CAINFO":         caBundle,
		"GIT_SSL_CAINFO":            caBundle,
		"MISE_DATA_DIR":             cacheDir + "/mise",
		"MISE_GLOBAL_CONFIG_FILE":   cacheDir + "/mise/cavu.toml",
		"MISE_TRUSTED_CONFIG_PATHS": "",
		"MISE_IGNORED_CONFIG_PATHS": workspaceDir,
		"PATH":                      defaultPath,
	}
}

// cacheVars sets each ecosystem's cache variables under root.
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
		case "rust":
			m["CARGO_HOME"] = path.Join(root, "cargo")
		}
	}
}

func render(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

// AgentEnv returns the environment that points each ecosystem at the read-only
// project cache and at writable task-local directories under home.
func AgentEnv(ecosystems []string, home string) []string {
	m := common()
	cacheVars(m, ecosystems, path.Join(home, ".cache"))

	for _, e := range ecosystems {
		if e == "go" {
			m["GOPROXY"] = "file://" + cacheDir + "/go/mod/cache/download,https://proxy.golang.org,direct"
			m["GOFLAGS"] = "-modcacherw"
		}
	}

	return render(m)
}

// SetupEnv returns the environment of the setup phase: caches under /cache, writable.
func SetupEnv(ecosystems []string) []string {
	m := common()
	cacheVars(m, ecosystems, cacheDir)

	return render(m)
}
