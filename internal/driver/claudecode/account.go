package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// oauthTokenSource is how the initialize answer names the subscription token.
const oauthTokenSource = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // a variable name, not a credential

// account is the account the initialize answer reports. The pinned release
// reports an API key that outranks the token as apiKeySource beside
// tokenSource.
type account struct {
	TokenSource  string `json:"tokenSource"`
	APIKeySource string `json:"apiKeySource"`
	APIProvider  string `json:"apiProvider"`
}

// accountMismatch says why the initialize answer's account does not fit the
// auth mode, or returns "" when it does. No account (another release) passes;
// one that cannot be read does not.
func accountMismatch(mode string, raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}

	var a account
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Sprintf("the initialize answer's account is unreadable: %v", err)
	}

	if mode == modeSubscription {
		if a.TokenSource == oauthTokenSource && a.APIProvider == "firstParty" &&
			(a.APIKeySource == "" || a.APIKeySource == "none") {
			return ""
		}

		return "subscription mode needs the OAuth token on the first-party API; " + a.String()
	}

	if a.TokenSource != oauthTokenSource {
		return ""
	}

	return "key mode must not use the OAuth token; " + a.String()
}

func (a account) String() string {
	return fmt.Sprintf("claude reports token source %q, API provider %q, API key source %q",
		a.TokenSource, a.APIProvider, a.APIKeySource)
}
