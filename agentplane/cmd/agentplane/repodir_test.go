package main

import "testing"

func TestRepoDirFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/foo/bar.git": "bar",
		"https://github.com/foo/bar":     "bar",
		"https://github.com/foo/bar/":    "bar",
		"git@github.com:foo/baz.git":     "baz",
		"":                               "repo",
	}
	for in, want := range cases {
		if got := repoDirFromURL(in); got != want {
			t.Errorf("repoDirFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
