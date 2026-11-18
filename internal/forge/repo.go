package forge

import (
	"errors"
	"net/url"
	"strings"
)

var errRepoAddress = errors.New("forge: a repository's address is https://HOST/OWNER/NAME, without credentials")

// Repo is a repository on a forge.
type Repo struct{ Host, Owner, Name string }

// ParseRepo reads https://HOST/OWNER/NAME, with or without .git. The host is
// kept in lower case and may carry a port.
func ParseRepo(rawURL string) (Repo, error) {
	// A parse error is not wrapped: it quotes the address, which may hold credentials.
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return Repo{}, errRepoAddress
	}

	parts := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, errRepoAddress
	}

	return Repo{Host: strings.ToLower(u.Host), Owner: parts[0], Name: parts[1]}, nil
}

// path is r's path under its forge's REST API.
func (r Repo) path() string { return "/repos/" + r.Owner + "/" + r.Name }

// restURL is the address of r's forge's REST API.
func (r Repo) restURL() string {
	switch {
	case r.Host == "github.com":
		return "https://api.github.com"
	case strings.HasSuffix(r.Host, ".ghe.com"):
		return "https://api." + r.Host
	default:
		return "https://" + r.Host + "/api/v3"
	}
}

// graphQLURL is the address of r's forge's GraphQL API.
func (r Repo) graphQLURL() string {
	if r.Host == "github.com" || strings.HasSuffix(r.Host, ".ghe.com") {
		return r.restURL() + "/graphql"
	}

	return "https://" + r.Host + "/api/graphql"
}
