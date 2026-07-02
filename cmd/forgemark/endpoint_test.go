package main

import "testing"

func TestVerbatimURLFor(t *testing.T) {
	tests := []struct {
		name string
		node string
		repo string
		want string
	}{
		{name: "full multi-segment path passed through, not inserted", node: "https://n/", repo: "pfx/acme/backend", want: "https://n/pfx/acme/backend"},
		{name: "generic keeps .git as given", node: "https://gh", repo: "you/x.git", want: "https://gh/you/x.git"},
		{name: "generic bare path (no forced .git)", node: "https://gh", repo: "you/x", want: "https://gh/you/x"},
		{name: "surrounding slashes trimmed", node: "https://n", repo: "/pfx/x/", want: "https://n/pfx/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verbatimURLFor(tt.node, tt.repo); got != tt.want {
				t.Fatalf("verbatimURLFor(%q, %q) = %q, want %q", tt.node, tt.repo, got, tt.want)
			}
		})
	}
}
