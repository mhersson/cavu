package claudecode

// anthropicHost is where the agent reaches its model with a subscription token.
const anthropicHost = "api.anthropic.com"

// Hosts returns a fresh slice on every call. Key mode needs none: the model
// route in the sidecar carries its traffic.
func (claudeCode) Hosts(mode string) []string {
	if mode == modeSubscription {
		return []string{anthropicHost}
	}

	return nil
}
