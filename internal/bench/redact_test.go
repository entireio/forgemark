package bench

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	body := `{"error":"invalid_grant","echoed":"eyJ-subject-token"}`
	got := redactSecrets(body, "eyJ-subject-token")
	if got == body {
		t.Fatalf("secret not redacted: %q", got)
	}
	if contains(got, "eyJ-subject-token") {
		t.Errorf("secret still present: %q", got)
	}
	// Empty secrets are ignored (an unset password must not blank the message).
	if out := redactSecrets("plain text", "", ""); out != "plain text" {
		t.Errorf("empty secrets altered the message: %q", out)
	}
}

// A misbehaving endpoint can echo the credential in any wire representation it
// saw, not just the literal secret: the base64 Basic authorization value, or
// the URL-encoded form-body rendition. authForms must cover them all.
func TestAuthFormsCoverEncodedRepresentations(t *testing.T) {
	user, pass := "token", "s3cr&t+pass"
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	body := fmt.Sprintf(`{"authorization":"Basic %s","form":"subject_token=s3cr%%26t%%2Bpass","raw":"%s"}`, basic, pass)
	got := redactSecrets(body, authForms(user, pass)...)
	for _, leak := range []string{pass, basic, "s3cr%26t%2Bpass"} {
		if contains(got, leak) {
			t.Errorf("credential representation %q survived redaction: %q", leak, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
