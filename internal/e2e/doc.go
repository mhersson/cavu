// Package e2e drives the built cavu from outside, as a person does: each test
// runs it as a child process on a data directory of its own, in key mode
// against the scripted mock Messages API, on a fixture repository served over
// TLS. Its tests carry the integration tag.
package e2e
