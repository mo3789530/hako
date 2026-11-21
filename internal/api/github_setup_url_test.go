package api

import "testing"

func TestValidGitHubSetupURL(t *testing.T) {
	for _, test := range []struct {
		value, prefix string
		want          bool
	}{
		{"https://github.com/apps/acme/installations/new?state=x", "/apps/", true},
		{"https://github.com/login/oauth/authorize?client_id=x", "/login/oauth/authorize", true},
		{"http://github.com/apps/acme/installations/new", "/apps/", false},
		{"https://github.com.attacker.invalid/apps/acme/installations/new", "/apps/", false},
		{"https://user@github.com/apps/acme/installations/new", "/apps/", false},
		{"https://github.com/evil", "/apps/", false},
		{"https://github.com/login/oauth/authorize#token", "/login/oauth/authorize", false},
	} {
		if got := validGitHubSetupURL(test.value, test.prefix); got != test.want {
			t.Errorf("validGitHubSetupURL(%q)=%v want %v", test.value, got, test.want)
		}
	}
}
