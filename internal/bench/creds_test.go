package bench

import (
	"context"
	"testing"
)

func TestStaticCredsBasicAuth(t *testing.T) {
	t.Parallel()
	c := staticCreds{username: "token", password: "sub.ject.jwt"}
	auth, err := c.basicAuth(context.Background(), "ignored/repo")
	if err != nil {
		t.Fatal(err)
	}
	if auth.Username != "token" || auth.Password != "sub.ject.jwt" {
		t.Fatalf("got %q/%q", auth.Username, auth.Password)
	}
}
