package env

import (
	"bytes"

	"github.com/BurntSushi/toml"
)

// MiseConfig renders the mise configuration CAVU writes. It holds only a
// [tools] table, encoded so no name or version can open another table: CAVU
// never writes [env], [tasks] or hooks. The encoder sorts keys.
func MiseConfig(tools []Tool) []byte {
	table := make(map[string]string, len(tools))
	for _, t := range tools {
		table[t.Name] = t.Version
	}

	var buf bytes.Buffer
	// Encoding a map of strings cannot fail.
	_ = toml.NewEncoder(&buf).Encode(map[string]map[string]string{"tools": table})

	return buf.Bytes()
}
