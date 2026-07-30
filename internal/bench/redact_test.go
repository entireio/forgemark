package bench

import "testing"

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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
