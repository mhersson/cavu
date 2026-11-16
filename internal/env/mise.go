package env

import (
	"bytes"
	"sort"

	"github.com/BurntSushi/toml"
)

// MiseConfig renders the mise configuration CAVU writes. It holds only a
// [tools] table, encoded so no name or version can open another table: CAVU
// never writes [env], [tasks] or hooks.
func MiseConfig(tools []Tool) []byte {
	sorted := append([]Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	table := make(map[string]string, len(sorted))
	for _, t := range sorted {
		table[t.Name] = t.Version
	}

	var buf bytes.Buffer
	// Encoding a map of strings cannot fail.
	_ = toml.NewEncoder(&buf).Encode(map[string]map[string]string{"tools": table})

	return buf.Bytes()
}
