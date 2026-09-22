package external

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	http2 "github.com/ReCasaOS/CasaOS-Common/utils/http"
)

// The internal secret.
//
// Every service listens on the loopback interface and, until now, trusted any
// request that came from it: the services talk to each other that way, and so
// did the gateway's management API. But loopback is not this box's services
// alone. A container on the host network, or any local account, reaches the
// same addresses, and a request there could install a compose file that mounts
// the root filesystem, read any file as root, reset the administrator's
// password, or re-route the dashboard's API. So loopback is no longer an
// identity. The gateway writes a random secret to the runtime path at every
// start, readable by root only, and a request is internal when it comes from
// loopback AND carries that secret in its Authorization header. The clients in
// this package send it on their own, to loopback addresses only, so a service
// that talks to another has nothing to do; anything else on the box needs a
// user's token, like the dashboard.

// InternalSecretFilename is the file in the runtime path that holds the secret
// of this boot.
const InternalSecretFilename = "internal.secret"

// internalScheme is the Authorization scheme the secret travels under; the JWT
// middleware of every service sees nothing it knows in it and refuses the
// request when the secret is wrong.
const internalScheme = "Internal "

// WriteInternalSecret writes a fresh secret for this boot, readable by root only.
func WriteInternalSecret(runtimePath string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}

	path := filepath.Join(runtimePath, InternalSecretFilename)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(raw)+"\n"), 0o600); err != nil {
		return err
	}

	// WriteFile keeps the mode of a file that already exists.
	return os.Chmod(path, 0o600)
}

// IsInternalRequest reports whether a request comes from one of this box's
// services: from loopback, with the secret of this boot.
func IsInternalRequest(realIP, authorization, runtimePath string) bool {
	if !isLoopbackIP(realIP) {
		return false
	}

	secret, err := readInternalSecret(runtimePath)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(authorization), []byte(internalScheme+secret)) == 1
}

func readInternalSecret(runtimePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(runtimePath, InternalSecretFilename))
	if err != nil {
		return "", err
	}

	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", errors.New("the internal secret file is empty")
	}

	return secret, nil
}

func isLoopbackIP(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1"
}

// useInternalSecret makes the HTTP helpers send this box's secret with every
// request to a loopback address. The file is read at each request, so a secret
// rewritten by a restarted gateway is picked up without anybody restarting.
func useInternalSecret(runtimePath string) {
	http2.SetInternalAuthorization(func() string { return InternalAuthorization(runtimePath) })
}

// InternalAuthorization returns the Authorization value that makes a request
// internal, "Internal <secret>", or "" when the secret cannot be read. It is for
// what a request editor does not fit, such as the Config.Header of a websocket
// handshake. Send it to loopback addresses only.
func InternalAuthorization(runtimePath string) string {
	secret, err := readInternalSecret(runtimePath)
	if err != nil {
		return ""
	}

	return internalScheme + secret
}

// InternalRequestEditor is for the API clients oapi-codegen generates, which do
// not go through the HTTP helpers of this module and so never sent the secret:
// pass it to their WithRequestEditorFn, and a request to one of this box's
// services carries it like every other internal call. Only loopback
// destinations get it, and a request that already has an Authorization header
// keeps its own.
func InternalRequestEditor(runtimePath string) func(ctx context.Context, req *http.Request) error {
	return func(_ context.Context, req *http.Request) error {
		if req.Header.Get("Authorization") != "" || !isLoopbackHost(req.URL.Hostname()) {
			return nil
		}
		if authorization := InternalAuthorization(runtimePath); authorization != "" {
			req.Header.Set("Authorization", authorization)
		}

		return nil
	}
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
