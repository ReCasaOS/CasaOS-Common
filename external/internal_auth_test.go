package external

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	http2 "github.com/ReCasaOS/CasaOS-Common/utils/http"
)

func writeSecretFor(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := WriteInternalSecret(dir); err != nil {
		t.Fatal(err)
	}
	secret, err := readInternalSecret(dir)
	if err != nil {
		t.Fatal(err)
	}

	return dir, secret
}

func TestTheSecretIsLongRandomAndRootOnly(t *testing.T) {
	dir, secret := writeSecretFor(t)
	if len(secret) != 64 || strings.Trim(secret, "0123456789abcdef") != "" {
		t.Fatalf("64 hex characters expected, got %q", secret)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, InternalSecretFilename))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o, want 600", info.Mode().Perm())
		}
	}

	if err := WriteInternalSecret(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := readInternalSecret(dir)
	if again == secret {
		t.Fatal("a new start must get a new secret")
	}
}

func TestOnlyLoopbackWithTheSecretIsInternal(t *testing.T) {
	dir, secret := writeSecretFor(t)
	right := internalScheme + secret

	for _, tc := range []struct {
		name         string
		ip, auth     string
		runtimePath  string
		wantInternal bool
	}{
		{"loopback v4 with the secret", "127.0.0.1", right, dir, true},
		{"loopback v6 with the secret", "::1", right, dir, true},
		{"the LAN, even with the secret", "192.168.1.20", right, dir, false},
		{"loopback with a wrong secret", "127.0.0.1", internalScheme + strings.Repeat("0", 64), dir, false},
		{"loopback with a bearer token", "127.0.0.1", "Bearer " + secret, dir, false},
		{"loopback with nothing", "127.0.0.1", "", dir, false},
		{"no secret file at all", "127.0.0.1", right, t.TempDir(), false},
	} {
		if got := IsInternalRequest(tc.ip, tc.auth, tc.runtimePath); got != tc.wantInternal {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.wantInternal)
		}
	}
}

func TestTheClientsSendTheSecretToLoopbackOnTheirOwn(t *testing.T) {
	dir, secret := writeSecretFor(t)
	useInternalSecret(dir)
	t.Cleanup(func() { http2.SetInternalAuthorization(nil) })

	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer server.Close()

	for _, call := range []func() (*http.Response, error){
		func() (*http.Response, error) { return http2.Get(server.URL, time.Second) },
		func() (*http.Response, error) { return http2.Post(server.URL, []byte("{}"), time.Second) },
		func() (*http.Response, error) { return http2.Put(server.URL, []byte("{}"), time.Second) },
		func() (*http.Response, error) { return http2.Delete(server.URL, []byte("{}"), time.Second) },
		func() (*http.Response, error) {
			return http2.GetWithHeader(server.URL, time.Second, map[string]string{"Authorization": "Bearer mine"})
		},
	} {
		response, err := call()
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}

	want := []string{internalScheme + secret, internalScheme + secret, internalScheme + secret, internalScheme + secret, "Bearer mine"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", seen, want)
	}
}

func TestTheRequestEditorSendsTheSecretToLoopbackOnly(t *testing.T) {
	dir, secret := writeSecretFor(t)
	edit := InternalRequestEditor(dir)

	for url, want := range map[string]string{
		"http://127.0.0.1:8080/v2/message_bus/event/x": internalScheme + secret,
		"http://[::1]:8080/v2/message_bus/event/x":     internalScheme + secret,
		"http://localhost:8080/v2/message_bus":         internalScheme + secret,
		"http://192.168.1.20:8080/v2/message_bus":      "",
		"https://example.com/v2/message_bus":           "",
	} {
		req := httptest.NewRequest(http.MethodPost, url, nil)
		if err := edit(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}

	mine := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/", nil)
	mine.Header.Set("Authorization", "Bearer mine")
	if err := edit(context.Background(), mine); err != nil || mine.Header.Get("Authorization") != "Bearer mine" {
		t.Fatalf("an Authorization already set must be kept, got %q (%v)", mine.Header.Get("Authorization"), err)
	}

	missing := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/", nil)
	if err := InternalRequestEditor(t.TempDir())(context.Background(), missing); err != nil || missing.Header.Get("Authorization") != "" {
		t.Fatalf("with no secret file the request goes without one, got %q (%v)", missing.Header.Get("Authorization"), err)
	}
}

func TestInternalAuthorizationIsTheHeaderValueOrNothing(t *testing.T) {
	dir, secret := writeSecretFor(t)
	if got := InternalAuthorization(dir); got != internalScheme+secret {
		t.Fatalf("got %q, want %q", got, internalScheme+secret)
	}

	if got := InternalAuthorization(t.TempDir()); got != "" {
		t.Fatalf("with no secret file, got %q", got)
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, InternalSecretFilename), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InternalAuthorization(empty); got != "" {
		t.Fatalf("with an empty secret file, got %q", got)
	}
}
