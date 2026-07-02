package main

import (
	"reflect"
	"testing"
)

func TestExpandRepos(t *testing.T) {
	tests := []struct {
		name     string
		reposCSV string
		pattern  string
		count    int
		want     []string
		wantErr  bool
	}{
		{name: "repos passthrough", reposCSV: "a/b,c/d", want: []string{"a/b", "c/d"}},
		{name: "pattern expands 1..N", pattern: "org/bench-{n}", count: 3,
			want: []string{"org/bench-1", "org/bench-2", "org/bench-3"}},
		{name: "pattern repeats {n}", pattern: "grp-{n}/bench-{n}", count: 2,
			want: []string{"grp-1/bench-1", "grp-2/bench-2"}},
		{name: "pattern keeps verbatim suffix", pattern: "you/bench-{n}.git", count: 2,
			want: []string{"you/bench-1.git", "you/bench-2.git"}},
		{name: "missing {n}", pattern: "bench-", count: 3, wantErr: true},
		{name: "pattern without count", pattern: "bench-{n}", count: 0, wantErr: true},
		{name: "count without pattern", count: 3, wantErr: true},
		{name: "repos plus count", reposCSV: "a/b", count: 3, wantErr: true},
		{name: "both repos and pattern", reposCSV: "a/b", pattern: "c-{n}", count: 2, wantErr: true},
		{name: "neither", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandRepos(tt.reposCSV, tt.pattern, tt.count)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expandRepos(%q,%q,%d) err = %v, wantErr %v", tt.reposCSV, tt.pattern, tt.count, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expandRepos(%q,%q,%d) = %v, want %v", tt.reposCSV, tt.pattern, tt.count, got, tt.want)
			}
		})
	}
}
