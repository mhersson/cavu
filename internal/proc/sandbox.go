package proc

import "strconv"

// SandboxMarker is an empty, root-owned, read-only file that only the sandbox
// image carries.
const SandboxMarker = "/etc/cavu/sandbox"

// NotInSandbox is the exit status of a script SandboxOnly refused.
const NotInSandbox = 125

// SandboxOnly returns a POSIX sh script that exits with NotInSandbox, running
// nothing of script, unless SandboxMarker exists. Callers pass every path as a
// positional argument and never interpolate one into script.
func SandboxOnly(script string) string {
	return "if [ ! -f " + SandboxMarker + " ]; then echo \"cavu: not in a sandbox container; nothing run\" >&2; exit " + strconv.Itoa(NotInSandbox) + "; fi\n" + script
}
