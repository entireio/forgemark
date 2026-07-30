package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
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
	url := verbatimURLFor(base, repo) + "/info/refs?service=git-receive-pack"
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
		return nil, fmt.Errorf("info/refs probe: HTTP %d: %s", resp.StatusCode, redactSecrets(strings.TrimSpace(string(body)), auth.Password))
	}

	if objectFmt == "auto" || objectFmt == "" {
		if strings.Contains(string(body), "object-format=sha256") {
			objFmt = formatcfg.SHA256
		}
	}

	nodes := SplitCSV(resp.Header.Get("X-Entire-Replicas"))
	if len(nodes) == 0 {
		nodes = []string{base} // single-node / dev: talk to the entry host
	}
	return &endpoint{nodes: nodes, objFmt: objFmt, label: base}, nil
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
