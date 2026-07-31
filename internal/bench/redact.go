package bench

import (
	"encoding/base64"
	"net/url"
	"strings"
)

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

// authForms returns every representation a basic-auth credential took on the
// wire, so all of them can be scrubbed from an echoed response: the raw
// secret, its URL-encoded form (as it appears in a form body or URL), and the
// complete `Authorization: Basic` value base64(user:pass). Redacting only the
// literal secret would let an endpoint that echoes the authorization header —
// or any encoded rendition of the request — leak the credential verbatim.
func authForms(user, pass string) []string {
	return []string{
		pass,
		url.QueryEscape(pass),
		base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
	}
}
