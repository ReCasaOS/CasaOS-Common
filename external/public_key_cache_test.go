package external

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
)

func TestTheKeyLastSeenValidatesWhileUserServiceIsDown(t *testing.T) {
	_, public, err := jwt.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := jwt.GenerateJwksJSON(public)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jwks)
	}))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UserServiceAddressFilename), []byte(server.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	cachedPublicKey, lastUpdate = nil, time.Time{}
	t.Cleanup(func() { cachedPublicKey, lastUpdate = nil, time.Time{} })

	seen, err := GetPublicKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seen.X.Cmp(public.X) != 0 {
		t.Fatal("the key user-service serves")
	}

	// user-service goes away, and the ten seconds pass
	server.Close()
	lastUpdate = time.Now().Add(-time.Minute)

	still, err := GetPublicKey(dir)
	if err != nil {
		t.Fatalf("the key last seen must serve while user-service is down: %v", err)
	}
	if still.X.Cmp(public.X) != 0 {
		t.Fatal("the same key")
	}

	// but a service that never saw one has nothing to check with
	cachedPublicKey = nil
	if _, err := GetPublicKey(dir); err == nil {
		t.Fatal("no key ever seen and user-service down: nothing can validate")
	}
}
