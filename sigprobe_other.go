//go:build !linux

package main

// probeSignals is a no-op off Linux. The SA_ONSTACK conflict it diagnoses is a
// POSIX-signal problem, and the Windows and macOS builds have no equivalent.
func probeSignals(string) {}
