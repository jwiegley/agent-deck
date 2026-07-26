package main

import "github.com/asheshgoplani/agent-deck/internal/session"

// normalizeRestartResult separates a real spawn/restart failure from a
// completed restart whose durability write failed. The latter must never send
// callers down rollback/retry paths; it remains an operator-visible warning.
func normalizeRestartResult(err error) (failure error, warning string) {
	if session.IsRestartPartialSuccess(err) {
		return nil, err.Error()
	}
	return err, ""
}

func mergeRestartWarnings(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "; " + second
}
