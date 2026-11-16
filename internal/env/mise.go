package env

import (
	"github.com/pelletier/go-toml/v2"
)

// MiseDataDir holds mise's toolchains and shims, on the project cache.
const MiseDataDir = cacheDir + "/mise"

// MiseConfigFile is where the setup phase writes MiseConfig's output, on the
// project cache; both phases point mise at it.
const MiseConfigFile = MiseDataDir + "/cavu.toml"

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
