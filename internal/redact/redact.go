// Package redact masks secret values, in plain, base64 and URL-encoded form,
// in text bound for the record.
package redact

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	minLen = 8
	mask   = "[redacted]"
)

// New returns a function that replaces each secret of 8 or more characters,
// and its base64 and URL-encoded forms, with "[redacted]". Longest first.
func New(secrets ...string) func(string) string {
	seen := map[string]bool{}

	var forms []string

	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			forms = append(forms, f)
		}
	}

	for _, s := range secrets {
		if utf8.RuneCountInString(s) < minLen {
			continue
		}

		add(s)
		add(base64.StdEncoding.EncodeToString([]byte(s)))
		add(url.QueryEscape(s))
	}

	// Longest first so a secret that prefixes another cannot leave a tail.
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })

	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, mask)
	}

	if len(pairs) == 0 {
		return func(s string) string { return s }
	}

	return strings.NewReplacer(pairs...).Replace
}
