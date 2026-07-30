package bench

import "strings"

// redactSecrets removes any occurrence of the given secret values from s,
// replacing each with a placeholder. Errors built from authenticated HTTP
// response bodies (the OAuth token exchange, the entiredb info/refs probe) are
// surfaced to the GUI over SSE and persisted in the result doc, so scrub the
// credential that was in scope for that request out of the text first — a
// misbehaving endpoint that echoes the token back can't then leak it.
func redactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return s
}
