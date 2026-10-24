package jwt

import "testing"

func TestTokenFromAuthorization(t *testing.T) {
	for header, want := range map[string]string{
		"":                       "",
		"a.b.c":                  "a.b.c",
		"  a.b.c  ":              "a.b.c",
		"Bearer a.b.c":           "a.b.c",
		"bearer a.b.c":           "a.b.c",
		"BEARER   a.b.c ":        "a.b.c",
		"Bearer":                 "Bearer",
		"Bearer ":                "Bearer",
		"Internal 0123abcd":      "Internal 0123abcd",
		"Basic dXNlcjpwYXNz":     "Basic dXNlcjpwYXNz",
		"Bearer a.b.c extra.d.e": "a.b.c extra.d.e",
		// the scheme once: what follows is the token, rejected by the check
		"Bearer Bearer a.b.c": "Bearer a.b.c",
		// a tab is not the separator (RFC 7235 SP): the value whole, rejected by the check
		"Bearer\ta.b.c": "Bearer\ta.b.c",
	} {
		if got := TokenFromAuthorization(header); got != want {
			t.Errorf("TokenFromAuthorization(%q) = %q, want %q", header, got, want)
		}
	}
}
