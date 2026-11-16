package env

import (
	"github.com/pelletier/go-toml/v2"
)

// MiseConfig renders the mise configuration CAVU writes. It holds only a
// [tools] table, encoded so no name or version can open another table: CAVU
// never writes [env], [tasks] or hooks. The encoder sorts keys.
func MiseConfig(tools []Tool) []byte {
	table := make(map[string]string, len(tools))
	for _, t := range tools {
		table[t.Name] = t.Version
	}

	// Marshalling a map of strings cannot fail: invalid UTF-8 is escaped.
	b, _ := toml.Marshal(map[string]map[string]string{"tools": table})

	return b
}
