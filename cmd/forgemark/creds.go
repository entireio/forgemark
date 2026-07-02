package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"golang.org/x/sync/singleflight"
)

// credentialProvider supplies the git HTTP basic-auth credential for a push.
// Two implementations cover every forge:
//
//	staticCreds       — a constant username/password (a GitHub/GitLab PAT, a
//	                    Gitea token, or plain basic auth). The common case.
//	jurisdictionCreds — the entiredb identity-token exchange: one short-lived
//	                    jurisdiction token for the whole region, refreshed
//	                    before it expires. Repo-independent, so the repo
//	                    argument is ignored.
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

// jurisdictionCreds exchanges a subject token (a session/SA JWT carrying
// entire:session) for an entiredb jurisdiction token and caches the single
// result, refreshing before expiry. Unlike a per-repo push token, one
// jurisdiction token authorizes every repo the caller can reach — the data
// plane resolves each push live — so there's no per-repo audience and no
// per-repo cache. The exchange is the RFC 8693 shape: audience is the bare
// jurisdiction origin (e.g. https://us.example.com), scope is openid.
type jurisdictionCreds struct {
	httpc    *http.Client
	tokenURL string // core auth /oauth/token, e.g. https://us.auth.example.com/oauth/token
	audience string // jurisdiction host bare origin, e.g. https://us.example.com
	clientID string // public OAuth client, e.g. entire-cli
	subject  string // exchangeable subject JWT (must carry entire:session)
	username string // basic-auth username; Entire ignores it

	mu         sync.Mutex
	tok        tokenEntry
	refreshing bool               // a background ahead-of-expiry refresh is in flight
	sf         singleflight.Group // collapses concurrent cold exchanges into one
}

type tokenEntry struct {
	token string
	exp   time.Time
}

// singleflightKey is arbitrary: there is exactly one token, so all callers
// coalesce onto the same key.
const singleflightKey = "jurisdiction"

func newJurisdictionCreds(httpc *http.Client, tokenURL, audience, clientID, subject, username string) *jurisdictionCreds {
	return &jurisdictionCreds{
		httpc:    httpc,
		tokenURL: tokenURL,
		audience: strings.TrimRight(audience, "/"),
		clientID: clientID,
		subject:  subject,
		username: username,
	}
}

func (j *jurisdictionCreds) basicAuth(ctx context.Context, _ string) (*githttp.BasicAuth, error) {
	token, err := j.get(ctx)
	if err != nil {
		return nil, err
	}
	// Entire ignores the username; the token is the password.
	return &githttp.BasicAuth{Username: j.username, Password: token}, nil
}

// get returns a valid jurisdiction token. A still-valid token within 2 minutes
// of expiry is returned immediately while a single background refresh runs
// (serve-stale-while-revalidate): a mid-sweep refresh must not block every
// agent at once, or it injects a synchronized spike into the p99/max tail this
// harness reports. Only a missing/expired token blocks — and singleflight
// collapses those concurrent first-time exchanges (all agents at startup) into
// one /oauth call instead of a herd.
func (j *jurisdictionCreds) get(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("jurisdiction token fetch canceled: %w", err)
	}
	j.mu.Lock()
	e := j.tok
	valid := e.token != "" && time.Now().Before(e.exp)
	// Only a valid-but-aging token triggers an ahead-refresh. Guarding on
	// `valid` matters: an expired token also satisfies time.Until < 2m, and
	// setting refreshing=true here without entering the goroutine that clears
	// it would leak the flag and permanently disable ahead-refresh.
	startAhead := valid && time.Until(e.exp) < 2*time.Minute && !j.refreshing
	if startAhead {
		j.refreshing = true
	}
	j.mu.Unlock()

	if valid {
		if startAhead {
			go func() {
				defer func() {
					j.mu.Lock()
					j.refreshing = false
					j.mu.Unlock()
				}()
				// Best-effort: the cached token is still valid, so a failed
				// ahead-refresh just retries on a later get; surface it but
				// don't disrupt the sweep.
				if _, err := j.refreshNow(); err != nil {
					fmt.Fprintf(os.Stderr, "forgemark: background jurisdiction-token refresh failed: %v\n", err)
				}
			}()
		}
		return e.token, nil
	}
	return j.refreshNow() // no valid token: block on the collapsed exchange
}

// refreshNow exchanges a fresh token and caches it, with singleflight
// collapsing concurrent calls into one network exchange. The exchange runs on
// a detached, time-bounded context so a single caller's cancellation can't
// fail a refresh other agents are waiting on.
func (j *jurisdictionCreds) refreshNow() (string, error) {
	v, err, _ := j.sf.Do(singleflightKey, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e, err := j.exchange(ctx)
		if err != nil {
			return "", err
		}
		j.mu.Lock()
		j.tok = e
		j.mu.Unlock()
		return e.token, nil
	})
	if err != nil {
		return "", fmt.Errorf("jurisdiction token exchange: %w", err)
	}
	token, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("jurisdiction token exchange: unexpected result type %T", v)
	}
	return token, nil
}

func (j *jurisdictionCreds) exchange(ctx context.Context) (tokenEntry, error) {
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token":        {j.subject},
		"audience":             {j.audience},
		"scope":                {"openid"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenEntry{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(j.clientID, "") // public client: empty secret is intentional

	resp, err := j.httpc.Do(req)
	if err != nil {
		return tokenEntry{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenEntry{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return tokenEntry{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var jr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &jr); err != nil {
		return tokenEntry{}, fmt.Errorf("decode: %w", err)
	}
	if jr.AccessToken == "" {
		return tokenEntry{}, fmt.Errorf("empty access_token")
	}
	ttl := jr.ExpiresIn
	if ttl <= 0 {
		ttl = 899 // ~15m default matching the jurisdiction-token TTL
	}
	return tokenEntry{token: jr.AccessToken, exp: time.Now().Add(time.Duration(ttl) * time.Second)}, nil
}
