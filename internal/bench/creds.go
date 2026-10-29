package bench

import (
	"context"

	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

// credentialProvider supplies the git HTTP basic-auth credential for a push.
// staticCreds covers every forge: a constant username/password (a
// GitHub/GitLab PAT, a Gitea token, plain basic auth, or the entiredb account
// access token, which the data plane authorizes live per push). The repo
// argument is ignored.
type credentialProvider interface {
	basicAuth(ctx context.Context, repo string) (*githttp.BasicAuth, error)
}

// staticCreds returns the same credential for every push. Username is a
// forge-specific placeholder for token auth (github ignores it and treats the
// password as the token; gitlab accepts any username with a PAT).
type staticCreds struct {
	username string
	password string
}

func (s staticCreds) basicAuth(context.Context, string) (*githttp.BasicAuth, error) {
	return &githttp.BasicAuth{Username: s.username, Password: s.password}, nil
}
