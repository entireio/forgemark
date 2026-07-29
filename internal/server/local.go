// CLI-sourced credentials and target discovery. The GUI is a loopback control
// panel on the operator's own machine, so instead of pasting a token into the
// form, a target can name a secret_source ("gh" | "glab" | "entire") and the
// server pulls the credential from the operator's already-authenticated CLI
// at run start. The token then never appears in any request or response body,
// and the browser never holds it at all.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const cliTimeout = 15 * time.Second

// credSource describes how to obtain a git credential from an authenticated
// CLI. Two shapes cover the forges: a token printer (gh, entire — stdout is
// the password, the username defaults to x-access-token) and a git-credential
// helper (glab — fed the target host on stdin, it replies with key=value lines
// carrying both a username and a git-scoped password, since GitLab's stored
// API token often lacks repository scope). Nothing user-controlled is ever
// executed: an unknown source is an error, never a command.
type credSource struct {
	argv    []string
	gitCred bool // speak the git credential-helper protocol over stdin/stdout
}

var secretSources = map[string]credSource{
	"gh":     {argv: []string{"gh", "auth", "token"}},
	"entire": {argv: []string{"entire", "auth", "token"}},
	"glab":   {argv: []string{"glab", "auth", "git-credential", "get"}, gitCred: true},
}

// resolveSecretSource returns (username, secret). username is "" when the
// source doesn't dictate one (the caller's default applies); it is set for
// git-credential helpers that name it (GitLab → oauth2). remote supplies the
// host a git-credential helper is queried for, so self-managed hosts work too.
func resolveSecretSource(source, remote string) (string, string, error) {
	c, ok := secretSources[source]
	if !ok {
		return "", "", fmt.Errorf("unknown secret_source %q (gh | glab | entire)", source)
	}
	stdin := ""
	if c.gitCred {
		stdin = fmt.Sprintf("protocol=https\nhost=%s\n\n", credHost(remote))
	}
	out, err := runCLI(stdin, c.argv...)
	if err != nil {
		return "", "", err
	}
	if c.gitCred {
		return parseGitCredential(out, strings.Join(c.argv, " "))
	}
	tok := strings.TrimSpace(out)
	if tok == "" {
		return "", "", fmt.Errorf("%s printed an empty token", strings.Join(c.argv, " "))
	}
	return "", tok, nil
}

// validateSecretAudience pins a CLI-sourced credential to its issuer so the
// server won't materialize it for an unrelated target. A gh/glab token belongs
// to its forge; the entire subject token is POSTed to token_url and the
// resulting jurisdiction token is used across the cluster, so remote, token_url
// and jurisdiction must all stay under entire.io. Self-hosted forges should
// paste a token rather than name a CLI source.
func validateSecretAudience(source, remote, tokenURL, jurisdiction string) error {
	// secureHost parses a URL that a CLI-sourced credential will be sent to and
	// returns its hostname only if the URL is safe to send a token over: it must
	// be https (a token as Basic auth over http:// leaks it on the wire) and
	// carry no userinfo (an embedded user:pass@ would override the credential the
	// audience check is pinning). Any deviation is an error, not a bare host, so
	// no downstream check can accidentally accept it.
	secureHost := func(label, raw string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("secret_source %s: %s %q is not a valid URL: %w", source, label, raw, err)
		}
		if u.Scheme != "https" {
			return "", fmt.Errorf("secret_source %s requires an https %s (a CLI token must never travel over cleartext), got %q", source, label, raw)
		}
		if u.User != nil {
			return "", fmt.Errorf("secret_source %s: %s must not embed userinfo, got %q", source, label, raw)
		}
		return u.Hostname(), nil
	}
	underEntire := func(h string) bool { return h == "entire.io" || strings.HasSuffix(h, ".entire.io") }
	switch source {
	case "gh":
		h, err := secureHost("remote", remote)
		if err != nil {
			return err
		}
		if h != "github.com" {
			return fmt.Errorf("secret_source gh is only valid for a github.com remote, not %q", remote)
		}
	case "glab":
		h, err := secureHost("remote", remote)
		if err != nil {
			return err
		}
		if h != "gitlab.com" {
			return fmt.Errorf("secret_source glab is only valid for a gitlab.com remote, not %q", remote)
		}
	case "entire":
		for label, raw := range map[string]string{"remote": remote, "token_url": tokenURL, "jurisdiction": jurisdiction} {
			h, err := secureHost(label, raw)
			if err != nil {
				return err
			}
			if !underEntire(h) {
				return fmt.Errorf("secret_source entire requires %s under entire.io, got %q", label, raw)
			}
		}
	default:
		return fmt.Errorf("unknown secret_source %q (gh | glab | entire)", source)
	}
	return nil
}

// credHost is the host a git-credential helper should be asked about, from the
// target remote; it falls back to gitlab.com so a bare "glab" source works.
func credHost(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Host
	}
	return "gitlab.com"
}

// parseGitCredential extracts username/password from a credential helper's
// key=value reply. Only exact "username="/"password=" lines match, so sibling
// keys like password_expiry_utc are ignored.
func parseGitCredential(out, label string) (string, string, error) {
	var user, pass string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "username="); ok {
			user = v
		} else if v, ok := strings.CutPrefix(strings.TrimSpace(line), "password="); ok {
			pass = v
		}
	}
	if pass == "" {
		return "", "", fmt.Errorf("%s returned no password (is it installed and logged in?)", label)
	}
	return user, pass, nil
}

func runCLI(stdin string, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return "", fmt.Errorf("%s: %s (is it installed and logged in?)", strings.Join(argv, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// suggestEntry is one CLI-discovered target template: a ready-to-post
// TargetSpec with SecretSource set and Secret empty, or the reason discovery
// failed. Note flags anything the operator still has to ensure themselves.
type suggestEntry struct {
	Target *TargetSpec `json:"target,omitempty"`
	Note   string      `json:"note,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// handleLocalSuggest builds target templates from the operator's gh, glab, and
// entire CLI logins, probed concurrently. Each entry is a ready-to-post target
// or the reason its CLI couldn't be reached; a CLI that isn't installed or
// logged in just yields an Error the UI shows as a dim note.
func (s *Server) handleLocalSuggest(w http.ResponseWriter, _ *http.Request) {
	var gh, gitlab, entire suggestEntry
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); gh = suggestGitHub() }()
	go func() { defer wg.Done(); gitlab = suggestGitLab() }()
	go func() { defer wg.Done(); entire = suggestEntire() }()
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]suggestEntry{"github": gh, "gitlab": gitlab, "entire": entire})
}

func suggestGitHub() suggestEntry {
	login, err := runCLI("", "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return suggestEntry{Error: err.Error()}
	}
	repo := login + "/forgemark-target"
	return suggestEntry{
		Target: &TargetSpec{
			Name:         "github",
			Remote:       "https://github.com",
			Repos:        []string{repo},
			SecretSource: "gh",
		},
		Note: fmt.Sprintf("repo %s must exist — scripts/compare-local.sh provisions it", repo),
	}
}

func suggestGitLab() suggestEntry {
	// glab has no --jq flag, so decode its JSON here.
	out, err := runCLI("", "glab", "api", "user")
	if err != nil {
		return suggestEntry{Error: err.Error()}
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(out), &u); err != nil {
		return suggestEntry{Error: fmt.Sprintf("decode glab api user: %v", err)}
	}
	if u.Username == "" {
		return suggestEntry{Error: "glab api user returned no username"}
	}
	// gitlab.com push URL is <base>/<namespace>/<project>.git. The credential
	// (username + git-scoped password) comes from glab's credential helper at
	// run start, so User is left blank here. The project must be created first.
	repo := u.Username + "/forgemark-target.git"
	return suggestEntry{
		Target: &TargetSpec{
			Name:         "gitlab",
			Remote:       "https://gitlab.com",
			Repos:        []string{repo},
			SecretSource: "glab",
		},
		Note: fmt.Sprintf("project %s must exist and be pushable — create it in GitLab first", repo),
	}
}

func suggestEntire() suggestEntry {
	// The two CLI calls are independent (the status output is only consulted
	// after both return), so run them concurrently — each can take up to
	// cliTimeout, and serial worst case would double the suggest latency.
	var (
		status, clustersJSON   string
		statusErr, clustersErr error
		wg                     sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); status, statusErr = runCLI("", "entire", "auth", "status") }()
	go func() { defer wg.Done(); clustersJSON, clustersErr = runCLI("", "entire", "api", "/api/v1/clusters") }()
	wg.Wait()
	if statusErr != nil {
		return suggestEntry{Error: statusErr.Error()}
	}
	if clustersErr != nil {
		return suggestEntry{Error: clustersErr.Error()}
	}

	slug := statusField(status, "Jurisdiction:")
	loginHost := statusField(status, "Context:")
	handle := strings.TrimPrefix(statusField(status, "User:"), "@")
	if slug == "" || loginHost == "" || handle == "" {
		return suggestEntry{Error: "could not parse `entire auth status` output (need Jurisdiction, Context, and User lines)"}
	}
	baseDomain := loginHost
	if i := strings.Index(loginHost, ".auth."); i >= 0 {
		baseDomain = loginHost[i+len(".auth."):]
	}
	var doc struct {
		Clusters []struct {
			Jurisdiction string `json:"jurisdiction"`
			PublicURL    string `json:"publicUrl"`
			IsDefault    bool   `json:"isDefault"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(clustersJSON), &doc); err != nil {
		return suggestEntry{Error: fmt.Sprintf("decode clusters: %v", err)}
	}
	remote := ""
	for _, c := range doc.Clusters {
		if c.Jurisdiction == slug && (remote == "" || c.IsDefault) {
			remote = c.PublicURL
		}
	}
	if remote == "" {
		return suggestEntry{Error: fmt.Sprintf("no cluster in jurisdiction %q", slug)}
	}

	repo := fmt.Sprintf("et/forgemark-%s/forgemark-target", handle)
	return suggestEntry{
		Target: &TargetSpec{
			Name:         "entire-" + slug,
			Remote:       remote,
			Repos:        []string{repo},
			TokenURL:     "https://" + loginHost + "/oauth/token",
			Jurisdiction: "https://" + slug + "." + baseDomain,
			SecretSource: "entire",
		},
		Note: fmt.Sprintf("repo %s must exist — FM_ENTIRE_NATIVE=1 scripts/compare-local.sh provisions it", repo),
	}
}

// statusField pulls the value after a "Label:" line prefix from the
// key-value block `entire auth status` prints.
func statusField(status, label string) string {
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, label); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}
