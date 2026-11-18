package forge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIAddresses(t *testing.T) {
	tests := []struct {
		url           string
		want          Repo
		rest, graphQL string
	}{
		{
			"https://github.com/o/n",
			Repo{"github.com", "o", "n"},
			"https://api.github.com", "https://api.github.com/graphql",
		},
		{
			"https://GitHub.com/o/n.git",
			Repo{"github.com", "o", "n"},
			"https://api.github.com", "https://api.github.com/graphql",
		},
		{
			"https://acme.ghe.com/o/n/",
			Repo{"acme.ghe.com", "o", "n"},
			"https://api.acme.ghe.com", "https://api.acme.ghe.com/graphql",
		},
		{
			"https://git.example.com:8443/o/n.git",
			Repo{"git.example.com:8443", "o", "n"},
			"https://git.example.com:8443/api/v3", "https://git.example.com:8443/api/graphql",
		},
	}

	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			r, err := ParseRepo(tc.url)
			require.NoError(t, err)
			assert.Equal(t, tc.want, r)
			assert.Equal(t, tc.rest, r.restURL())
			assert.Equal(t, tc.graphQL, r.graphQLURL())
		})
	}
}

func TestParseRepoRefuses(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/o/n",
		"ssh://git@github.com/o/n.git",
		"git@github.com:o/n.git",
		"https://user:secret@github.com/o/n",
		"https://x-access-token@github.com/o/n",
		"https:///o/n",
		"https://github.com/n",
		"https://github.com/o/n/x",
		"https://github.com/o/.git",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseRepo(raw)
			require.Error(t, err)
		})
	}
}
