package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"golang.org/x/net/publicsuffix"
)

// endpoint is a fully-resolved push destination: the node base URLs to fan out
// across, the wire object format, and a label for logs/results. It's built once
// at startup by a per-forge constructor. The git remote URL for a (node, repo)
// is always verbatimURLFor — the forges no longer differ on the URL side.
//
// A plain forge has one node (the remote host). Entire discovers its nodes
// from an info/refs probe, because a push must go direct-to-node (its load
// balancer only forwards info/refs, not git-receive-pack).
type endpoint struct {
	nodes  []string
	objFmt formatcfg.ObjectFormat
	label  string
}

// verbatimURLFor joins a node and a caller-supplied repo path with no
// rewriting: the path is used exactly as given in -repos / -repo-pattern, so
// the caller controls the full URL. Surrounding slashes are trimmed so exactly
// one separator is inserted between node and repo.
func verbatimURLFor(node, repo string) string {
	return strings.TrimRight(node, "/") + "/" + strings.Trim(repo, "/")
}

// newGenericEndpoint targets any smart-HTTP git host: one node (the -remote
// base), no discovery, and a URL of <base>/<repo> with the repo path appended
// verbatim (include a .git suffix in -repos if the forge needs it). Object
// format comes from the flag (default sha1 — most forges); pass -object-format
// sha256 for a sha256 remote. github is this with base=https://github.com.
func newGenericEndpoint(base string, objFmt formatcfg.ObjectFormat) *endpoint {
	base = strings.TrimRight(base, "/")
	return &endpoint{
		nodes:  []string{base},
		objFmt: objFmt,
		label:  base,
	}
}

// newEntireEndpoint probes Entire once to learn its nodes (X-Entire-Replicas)
// and object format, then pushes direct-to-node. The probe hits
// info/refs?service=git-receive-pack, which the load balancer does forward,
// using the caller's credential. The repo path is appended verbatim (the caller
// supplies the full path in -repos / -repo-pattern), same as a plain forge.
func newEntireEndpoint(ctx context.Context, remote, objectFmt, repo string, creds credentialProvider, httpc *http.Client) (*endpoint, error) {
	base := strings.TrimRight(remote, "/")

	// Object format: explicit flag wins; otherwise default sha1 (entiredb repos
	// today are sha1) and let the advertisement upgrade it to sha256 if seen.
	objFmt := formatcfg.SHA1
	switch objectFmt {
	case "sha256":
		objFmt = formatcfg.SHA256
	case "sha1", "auto", "":
	default:
		return nil, fmt.Errorf("invalid -object-format %q (sha1|sha256|auto)", objectFmt)
	}

	auth, err := creds.basicAuth(ctx, repo)
	if err != nil {
		return nil, err
	}
	// Hand redirects back instead of following them: Go would forward the
	// token to wherever Location points.
	probe := *httpc
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hdr, body, err := probeInfoRefs(ctx, &probe, base, repo, auth)
	if err != nil {
		return nil, err
	}
	advertised := hdr.Get("X-Entire-Replicas")
	if loc := hdr.Get("Location"); loc != "" {
		// Entire redirects discovery to a hosting replica, always so via
		// the ALB. Follow one hop, only to an advertised, pinned node.
		nodes, err := pinReplicas(base, SplitCSV(advertised))
		if err != nil {
			return nil, err
		}
		node, err := redirectNode(base, loc, nodes)
		if err != nil {
			return nil, err
		}
		if hdr, body, err = probeInfoRefs(ctx, &probe, node, repo, auth); err != nil {
			return nil, err
		}
		if hdr.Get("Location") != "" {
			return nil, fmt.Errorf("info/refs probe: replica %s redirected again; refusing to follow", node)
		}
		if h := hdr.Get("X-Entire-Replicas"); h != "" {
			advertised = h
		}
	}

	if objectFmt == "auto" || objectFmt == "" {
		if strings.Contains(string(body), "object-format=sha256") {
			objFmt = formatcfg.SHA256
		}
	}

	nodes, err := pinReplicas(base, SplitCSV(advertised))
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		nodes = []string{base} // single-node / dev: talk to the entry host
	}
	return &endpoint{nodes: nodes, objFmt: objFmt, label: base}, nil
}

// pinReplicas admits an advertised replica only when it shares the entry
// host's scheme, registrable domain (aws-us-east-2.entire.io admits
// *.entire.io) and, over https, effective port, and carries no userinfo.
// The account access token is presented to every node, so an unvalidated
// header could redirect it to an arbitrary origin. Any rejected replica
// fails the run. http (local dev) nodes may differ in port: each node
// listens on its own.
func pinReplicas(base string, nodes []string) ([]string, error) {
	bu, err := url.Parse(base)
	if err != nil || bu.Hostname() == "" {
		return nil, fmt.Errorf("remote %q is not a valid URL", base)
	}
	baseHost := strings.ToLower(bu.Hostname())
	site := registrableDomain(baseHost)
	for _, n := range nodes {
		u, err := url.Parse(n)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("X-Entire-Replicas entry %q is not a valid URL", n)
		}
		h := strings.ToLower(u.Hostname())
		sameHost := h == baseHost || (site != "" && (h == site || strings.HasSuffix(h, "."+site)))
		samePort := bu.Scheme != "https" || effectivePort(u) == effectivePort(bu)
		bareOrigin := (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
		if u.Scheme != bu.Scheme || u.User != nil || !sameHost || !samePort || !bareOrigin {
			return nil, fmt.Errorf("X-Entire-Replicas entry %q is not under %s; refusing to send the credential there", n, base)
		}
	}
	return nodes, nil
}

// probeInfoRefs GETs node's receive-pack advertisement for repo. It returns
// the headers and body of a 200, or the headers of a redirect (Location
// set) for the caller to vet; any other status is an error.
func probeInfoRefs(ctx context.Context, c *http.Client, node, repo string, auth *githttp.BasicAuth) (http.Header, []byte, error) {
	url := verbatimURLFor(node, repo) + "/info/refs?service=git-receive-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build info/refs request: %w", err)
	}
	req.SetBasicAuth(auth.Username, auth.Password)
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("info/refs probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read info/refs response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header, body, nil
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if resp.Header.Get("Location") == "" {
			return nil, nil, fmt.Errorf("info/refs probe: HTTP %d without Location", resp.StatusCode)
		}
		return resp.Header, nil, nil
	}
	// A 404 is the most common setup miss, so make it actionable — but
	// diagnose, don't assert: forges also answer 404 for repos the
	// credential simply can't see, so a "create it" order alone would
	// misdirect an auth problem. forgemark deliberately never creates
	// repos (a typo'd path must fail fast, not provision junk on a real
	// jurisdiction); the server body is kept as evidence either way.
	hint := ""
	if resp.StatusCode == http.StatusNotFound {
		hint = fmt.Sprintf(" — repo %s is missing on %s, or the credential can't see it. forgemark does not create repos: "+
			"create it first (`entire repo create` / `entire repo mirror create`) or fix the repo path / token scope", repo, node)
	}
	return nil, nil, fmt.Errorf("info/refs probe: HTTP %d: %s%s", resp.StatusCode,
		redactSecrets(strings.TrimSpace(string(body)), authForms(auth.Username, auth.Password)...), hint)
}

// redirectNode maps a discovery Location onto the pinned node it names.
// Userinfo (Entire embeds the caller's token) and path are dropped: the
// probe rebuilds the URL and presents its own credential.
func redirectNode(base, loc string, nodes []string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("remote %q is not a valid URL", base)
	}
	lu, err := bu.Parse(loc)
	if err != nil {
		return "", errors.New("info/refs probe: redirect Location is not a valid URL")
	}
	for _, n := range nodes {
		nu, err := url.Parse(n)
		if err == nil && strings.EqualFold(nu.Scheme, lu.Scheme) && strings.EqualFold(nu.Host, lu.Host) {
			return strings.TrimRight(n, "/"), nil
		}
	}
	return "", fmt.Errorf("info/refs probe: redirect to %s://%s is not an advertised replica; refusing to follow", lu.Scheme, lu.Host)
}

// registrableDomain is host's eTLD+1 per the public suffix list, so
// tenant.github.io stays apart from other.github.io. IPs and bare
// names like localhost have none: only the exact host matches.
func registrableDomain(host string) string {
	if net.ParseIP(host) != nil {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return d
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
