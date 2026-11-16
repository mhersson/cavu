package git

import (
	"net/url"
	"strconv"
)

const (
	tokenVar  = "CAVU_GIT_TOKEN" //nolint:gosec // the variable name, not a secret
	tokenUser = "x-access-token"
	// helperScript answers git's credential "get" with the token from the environment.
	helperScript = `!f() { test "$1" = get && printf "username=` + tokenUser + `\npassword=%s\n" "$` + tokenVar + `"; }; f`
)

// netEnv returns the environment of a network call. The token lives only in
// tokenVar, read by a helper scoped to rawURL's scheme and host, so it reaches
// no other host after a redirect, and no argv, file or hook. allowed lists the
// transport protocols git may use.
func netEnv(rawURL, token, allowed string) []string {
	kv := [][2]string{{"credential.helper", ""}}

	if u, err := url.Parse(rawURL); err == nil && token != "" && u.Host != "" {
		kv = append(kv, [2]string{"credential." + u.Scheme + "://" + u.Host + ".helper", helperScript})
	}

	env := []string{"LC_ALL=C", "GIT_ALLOW_PROTOCOL=" + allowed, "GIT_CONFIG_COUNT=" + strconv.Itoa(len(kv))}

	if token != "" {
		env = append(env, tokenVar+"="+token)
	}

	for i, p := range kv {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}

	return env
}
