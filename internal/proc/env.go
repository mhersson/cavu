package proc

import (
	"slices"
	"strings"
)

// BuildEnv returns base, then every key in set (overriding base), set keys
// appended in sorted order.
func BuildEnv(base []string, set map[string]string) []string {
	out := make([]string, 0, len(base)+len(set))

	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, overridden := set[key]; overridden {
			continue
		}

		out = append(out, kv)
	}

	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}

	return out
}
