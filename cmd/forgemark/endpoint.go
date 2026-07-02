package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

// endpoint is a fully-resolved push destination: the node base URLs to fan out
// across, the wire object format, a builder that turns a (node, repo) into a
// git remote URL, and a label for logs/results. It's built once at startup by
// a per-forge constructor — the only place the forges differ on the URL side.
//
// A plain forge has one node (the remote host) and a literal URL builder.
// Entire discovers its nodes from an info/refs probe, because a push must go
// direct-to-node (its load balancer only forwards info/refs, not
// git-receive-pack), and prefixes repos with /et/.
type endpoint struct {
	nodes  []string
	objFmt formatcfg.ObjectFormat
	urlFor func(node, repo string) string
	label  string
}

// newGenericEndpoint targets any smart-HTTP git host: one node (the -remote
// base), no discovery, and a literal URL of <base>/<owner>/<repo>.git. Object
// format comes from the flag (default sha1 — most forges); pass -object-format
// sha256 for a sha256 remote. github is this with base=https://github.com.
func newGenericEndpoint(base string, objFmt formatcfg.ObjectFormat) *endpoint {
	base = strings.TrimRight(base, "/")
	return &endpoint{
		nodes:  []string{base},
		objFmt: objFmt,
		urlFor: func(node, repo string) string {
			return node + "/" + strings.TrimSuffix(repo, ".git") + ".git"
		},
		label: base,
	}
}

// newEntireEndpoint probes Entire once to learn its nodes (X-Entire-Replicas)
// and object format, then pushes direct-to-node under the /et/ path. The probe
// hits info/refs?service=git-receive-pack, which the load balancer does
// forward, using the caller's credential.
func newEntireEndpoint(ctx context.Context, cfg *runConfig, creds credentialProvider, httpc *http.Client) (*endpoint, error) {
	base := strings.TrimRight(cfg.remote, "/")
	// /et/ (native repos) is hardcoded on purpose — don't make it configurable.
	// A /gh/ mirror would forward the push to github.com, which is neither what
	// we're benchmarking nor something to do to a real GitHub repo.
	urlFor := func(node, repo string) string {
		return strings.TrimRight(node, "/") + "/et/" + repo
	}

	// Object format: explicit flag wins; otherwise default sha1 (entiredb repos
	// today are sha1) and let the advertisement upgrade it to sha256 if seen.
	objFmt := formatcfg.SHA1
	switch cfg.objectFmt {
	case "sha256":
		objFmt = formatcfg.SHA256
	case "sha1", "auto", "":
	default:
		return nil, fmt.Errorf("invalid -object-format %q (sha1|sha256|auto)", cfg.objectFmt)
	}

	auth, err := creds.basicAuth(ctx, cfg.repos[0])
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/et/%s/info/refs?service=git-receive-pack", base, cfg.repos[0])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build info/refs request: %w", err)
	}
	req.SetBasicAuth(auth.Username, auth.Password)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("info/refs probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read info/refs response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("info/refs probe: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if cfg.objectFmt == "auto" || cfg.objectFmt == "" {
		if strings.Contains(string(body), "object-format=sha256") {
			objFmt = formatcfg.SHA256
		}
	}

	nodes := splitCSV(resp.Header.Get("X-Entire-Replicas"))
	if len(nodes) == 0 {
		nodes = []string{base} // single-node / dev: talk to the entry host
	}
	return &endpoint{nodes: nodes, objFmt: objFmt, urlFor: urlFor, label: base}, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
