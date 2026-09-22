package external

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestTheMessageBusSocketIsInTheRuntimePath(t *testing.T) {
	if got, want := MessageBusSocketPath("/var/run/casaos"), filepath.Join("/var/run/casaos", "message-bus.sock"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPublishEventInSocketPostsToTheRuntimeSocket(t *testing.T) {
	// Short: a unix socket path is limited to about a hundred bytes.
	runtimePath, err := os.MkdirTemp("", "mb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimePath) })

	listener, err := net.Listen("unix", MessageBusSocketPath(runtimePath))
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}

	type request struct {
		method, path, contentType string
		properties                map[string]string
	}
	received := make(chan request, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := request{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type")}
		json.NewDecoder(r.Body).Decode(&got.properties)
		received <- got
		w.WriteHeader(http.StatusAccepted)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	response, err := PublishEventInSocket(context.Background(), runtimePath, "app-management", "app:install-end", map[string]string{"app:name": "nextcloud"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want %d", response.StatusCode, http.StatusAccepted)
	}

	got := <-received
	if got.method != http.MethodPost || got.path != "/v2/message_bus/event/app-management/app:install-end" || got.contentType != "application/json" {
		t.Errorf("got %s %s (%s)", got.method, got.path, got.contentType)
	}
	if got.properties["app:name"] != "nextcloud" {
		t.Errorf("got properties %v", got.properties)
	}

	if _, err := PublishEventInSocket(context.Background(), t.TempDir(), "app-management", "app:install-end", nil); err == nil {
		t.Fatal("a runtime path without the message bus must fail")
	}
}
