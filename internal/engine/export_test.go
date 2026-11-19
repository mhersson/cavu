package engine

// SetPassHook makes s call f at the end of each pass.
func SetPassHook(s *Scheduler, f func()) { s.passed = f }

// Counted is counted, for the tests of the telling.
var Counted = counted
