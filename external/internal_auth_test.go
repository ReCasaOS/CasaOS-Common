package external

import (
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
