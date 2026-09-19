package config

import "testing"

func TestParsePublicBaseURL(t *testing.T) {
	cases := []struct{ explicit, railway, want string }{
		{"https://api.champion.example", "", "https://api.champion.example"},
		{"https://api.champion.example/", "", "https://api.champion.example"},
		{" http://localhost:8080 ", "", "http://localhost:8080"},
		{"https://explicit.example", "railway.up.app", "https://explicit.example"}, // explicit wins
		{"", "dayzkillfeedbot-production.up.railway.app", "https://dayzkillfeedbot-production.up.railway.app"},
		{"", "", ""},
		// An explicit value that is not a bare http(s) origin is ignored (and does NOT fall back).
		{"ftp://x.example", "railway.up.app", ""},
		{"javascript:alert(1)", "", ""},
		{"https://user:pw@x.example", "", ""},
		{"https://x.example/some/path", "", ""},
		{"https://x.example?q=1", "", ""},
		{"https://x.example#frag", "", ""},
		{"//x.example", "", ""},
		{"not a url", "", ""},
		// A malformed Railway domain is ignored.
		{"", "evil.example/path", ""},
		{"", "evil.example:8080", ""},
		{"", "a b", ""},
	}
	for _, c := range cases {
		if got := ParsePublicBaseURL(c.explicit, c.railway); got != c.want {
			t.Errorf("ParsePublicBaseURL(%q, %q) = %q, want %q", c.explicit, c.railway, got, c.want)
		}
	}
}
