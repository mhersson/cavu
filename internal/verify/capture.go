package verify

// Argv wraps a declared command for execution: bash -c "set -o pipefail; <cmd>".
func Argv(command string) []string {
	return []string{"bash", "-c", "set -o pipefail; " + command}
}
