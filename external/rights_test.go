package external

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// rightsServer answers every role request with the raw body it holds, and counts them.
func rightsServer(t *testing.T, body *atomic.Value) (dir string, hits *int32, clock *time.Time, server *httptest.Server) {
	t.Helper()
	hits = new(int32)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(server.Close)
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UserServiceAddressFilename), []byte(server.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock = &now
	roleNow = func() time.Time { return *clock }
	roleTable = map[int]roleEntry{}
	t.Cleanup(func() { roleNow = time.Now; roleTable = map[int]roleEntry{} })
	return dir, hits, clock, server
}

func replyOf(s string) *atomic.Value {
	v := new(atomic.Value)
	v.Store(s)
	return v
}

func TestRightsOfReadsTheRightsWithTheRoleInOneRequest(t *testing.T) {
	dir, hits, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user","rights":["storage","vm"]}`))
	if RoleOf(dir, 4) != "user" {
		t.Fatal("user expected")
	}
	got := RightsOf(dir, "4")
	if !got["storage"] || !got["vm"] || got["fleet"] || len(got) != 2 {
		t.Fatalf("rights: %v", got)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("one request for the role and the rights, got %d", atomic.LoadInt32(hits))
	}
}

func TestRightsOfIsEmptyWhenUserServiceIsDown(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UserServiceAddressFilename), []byte("http://127.0.0.1:1"), 0o600); err != nil {
		t.Fatal(err)
	}
	roleTable = map[int]roleEntry{}
	t.Cleanup(func() { roleTable = map[int]roleEntry{} })
	if got := RightsOf(dir, "4"); got == nil || len(got) != 0 {
		t.Fatalf("rights with no answer: %v", got)
	}
	if got := RightsOf(dir, "x"); got == nil || len(got) != 0 {
		t.Fatalf("rights of a bad id: %v", got)
	}
}

func TestRightsOfParsesAReplyWithoutRights(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user"}`))
	if got := RightsOf(dir, "4"); len(got) != 0 {
		t.Fatalf("rights of an old reply: %v", got)
	}
	if RoleOf(dir, 4) != "user" {
		t.Fatal("the role must still be read")
	}
}

func TestRightsOfIgnoresUnknownNames(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user","rights":["root","vm","Storage",""]}`))
	got := RightsOf(dir, "4")
	if len(got) != 1 || !got["vm"] {
		t.Fatalf("rights: %v", got)
	}
}

func TestRightsOfIsEmptyForAdminAndGoneAccounts(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"admin","rights":["vm"]}`))
	if got := RightsOf(dir, "1"); len(got) != 0 {
		t.Fatalf("admin rights: %v", got)
	}
}

func TestRightsAreEmptyAsSoonAsTheAnswerIsNotFresh(t *testing.T) {
	dir, _, clock, server := rightsServer(t, replyOf(`{"success":200,"data":"user","rights":["backups"]}`))
	if !RightsOf(dir, "4")["backups"] {
		t.Fatal("granted right expected")
	}
	*clock = clock.Add(2 * time.Second)
	if !RightsOf(dir, "4")["backups"] {
		t.Fatal("a fresh answer still gives the right")
	}
	server.Close()
	*clock = clock.Add(4 * time.Minute)
	if got := RightsOf(dir, "4"); got == nil || len(got) != 0 {
		t.Fatalf("rights with user-service down: %v", got)
	}
	if RoleOf(dir, 4) != "user" {
		t.Fatal("the role keeps its stale grace")
	}
}

func TestAChangeToFewerRightsIsPickedUpAfterFiveSeconds(t *testing.T) {
	body := replyOf(`{"success":200,"data":"user","rights":["storage","system"]}`)
	dir, _, clock, _ := rightsServer(t, body)
	if got := RightsOf(dir, "4"); len(got) != 2 {
		t.Fatalf("rights: %v", got)
	}
	body.Store(`{"success":200,"data":"user","rights":["storage"]}`)
	if got := RightsOf(dir, "4"); len(got) != 2 {
		t.Fatalf("still the cached answer inside five seconds: %v", got)
	}
	*clock = clock.Add(6 * time.Second)
	if got := RightsOf(dir, "4"); len(got) != 1 || !got["storage"] {
		t.Fatalf("rights after the change: %v", got)
	}
}

func TestValidRightsAreTheFiveModules(t *testing.T) {
	got := ValidRights()
	want := []string{"storage", "vm", "backups", "fleet", "system"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ValidRights: %v", got)
	}
	got[0] = "x"
	if ValidRights()[0] != "storage" {
		t.Fatal("the list must be a copy")
	}
}
