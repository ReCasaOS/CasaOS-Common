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
	} {
		if got := TokenFromAuthorization(header); got != want {
			t.Errorf("TokenFromAuthorization(%q) = %q, want %q", header, got, want)
		}
	}
}
