package jwt

import "strings"

// TokenFromAuthorization is the user token in an Authorization header value:
// the bare token (as CasaOS clients have always sent it), or the standard
// `Bearer <token>` (RFC 6750; the scheme is case-insensitive, RFC 7235).
// Every other scheme, `Internal <secret>` among them, is returned as it came:
// it parses as no token.
func TokenFromAuthorization(authorization string) string {
	value := strings.TrimSpace(authorization)
	scheme, rest, found := strings.Cut(value, " ")
	if found && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(rest)
	}
	return value
}
