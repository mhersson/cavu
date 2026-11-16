//go:build unix && !linux

package proc

// killDescendants needs /proc; elsewhere the group kill is all there is.
func killDescendants(int) {}
