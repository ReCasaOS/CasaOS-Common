package external

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/common_err"
)

// roleServer answers every role request with status and data, and counts them.
func roleServer(t *testing.T, status int, data string) (dir string, hits *int32, clock *time.Time, server *httptest.Server) {
	t.Helper()
	hits = new(int32)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		success := status
		if status == http.StatusNotFound {
			success = common_err.USER_NOT_EXIST // what user-service says of an account it does not have
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"success":%d,"message":"ok","data":%q}`, success, data)
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

func TestRoleIsCachedFiveSeconds(t *testing.T) {
	dir, hits, clock, _ := roleServer(t, 200, "admin")

	if RoleOf(dir, 1) != "admin" || RoleOf(dir, 1) != "admin" {
		t.Fatal("admin expected")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("one request in five seconds, got %d", atomic.LoadInt32(hits))
	}
	*clock = clock.Add(6 * time.Second)
	RoleOf(dir, 1)
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("a new request after five seconds, got %d", atomic.LoadInt32(hits))
	}
}

func TestRoleKeepsLastKnownValueUpToFiveMinutesWhenUserServiceIsDown(t *testing.T) {
	dir, _, clock, server := roleServer(t, 200, "admin")
	RoleOf(dir, 1)
	server.Close()

	*clock = clock.Add(4 * time.Minute)
	if got := RoleOf(dir, 1); got != "admin" {
		t.Fatalf("last known value expected, got %q", got)
	}
	*clock = clock.Add(2 * time.Minute)
	if got := RoleOf(dir, 1); got != "admin" {
		t.Fatalf("an admin answer never expires, got %q", got)
	}
}

func TestAdminAnswerNeverExpires(t *testing.T) {
	dir, _, clock, server := roleServer(t, 200, "admin")
	RoleOf(dir, 1)
	server.Close()
	*clock = clock.Add(24 * time.Hour)
	if got, ok := RoleKnown(dir, "1"); got != "admin" || !ok {
		t.Fatalf("admin expected, got %q %v", got, ok)
	}
}

func TestUserAnswerStillExpires(t *testing.T) {
	dir, _, clock, server := roleServer(t, 200, "user")
	if got, ok := RoleKnown(dir, "3"); got != "user" || !ok {
		t.Fatalf("user expected, got %q %v", got, ok)
	}
	server.Close()
	*clock = clock.Add(4 * time.Minute)
	if _, ok := RoleKnown(dir, "3"); !ok {
		t.Fatal("still known inside five minutes")
	}
	*clock = clock.Add(2 * time.Minute)
	if got, ok := RoleKnown(dir, "3"); got != "" || ok {
		t.Fatalf("expired user must be unknown, got %q %v", got, ok)
	}
}

func TestRoleKnownFalseBeforeAnyAnswer(t *testing.T) {
	dir, _, _, server := roleServer(t, 200, "admin")
	server.Close()
	if got, ok := RoleKnown(dir, "1"); got != "" || ok {
		t.Fatalf("unknown expected, got %q %v", got, ok)
	}
}

func TestRoleKnownRejectsInvalidIDs(t *testing.T) {
	dir, hits, _, _ := roleServer(t, 200, "admin")
	for _, id := range []string{"", "0", "-1", "abc", "1x", "99999999999999999999"} {
		if got, ok := RoleKnown(dir, id); got != "" || ok {
			t.Fatalf("%q: got %q %v", id, got, ok)
		}
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("an invalid id must not be looked up, %d requests", atomic.LoadInt32(hits))
	}
}

func TestPositiveUserAnswerReplacesAdmin(t *testing.T) {
	dir, _, clock, _ := roleServer(t, 200, "user")
	roleTable[1] = roleEntry{role: "admin", at: roleNow().Add(-time.Hour)}
	*clock = clock.Add(time.Second)
	if got, ok := RoleKnown(dir, "1"); got != "user" || !ok {
		t.Fatalf("user expected, got %q %v", got, ok)
	}
}

func TestRoleIsUserWhenNothingIsKnown(t *testing.T) {
	dir, _, _, server := roleServer(t, 200, "admin")
	server.Close()
	if got := RoleOf(dir, 1); got != "user" {
		t.Fatalf("user expected, got %q", got)
	}
	if got := RoleOf(t.TempDir(), 2); got != "user" {
		t.Fatalf("no address file: user expected, got %q", got)
	}
}

func TestDeletedAccountIsUser(t *testing.T) {
	dir, hits, _, _ := roleServer(t, 404, "")
	roleTable[1] = roleEntry{role: "admin", at: roleNow().Add(-time.Minute)}

	if got := RoleOf(dir, 1); got != "user" {
		t.Fatalf("user expected, got %q", got)
	}
	RoleOf(dir, 1)
	if atomic.LoadInt32(hits) != 1 || roleTable[1].role != "user" {
		t.Fatalf("the 404 must be cached as user (hits %d)", atomic.LoadInt32(hits))
	}
	if !AccountGone(dir, 1) || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("the account is gone, from the same cached answer (hits %d)", atomic.LoadInt32(hits))
	}
}

// A 404 whose body is not user-service's word for a missing account (an older user-service
// without the role route, a proxy page) is no answer: the last known role holds.
func TestA404WithoutTheMissingAccountCodeIsAFailure(t *testing.T) {
	for _, body := range []string{`{"message":"Not Found"}`, `{"success":404,"message":"x"}`, `404 page not found`, ``} {
		var hits int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, body)
		}))
		dir := writeAddress(t, server.URL)
		now := time.Now()
		roleNow = func() time.Time { return now }
		roleTable[1] = roleEntry{role: "admin", at: now.Add(-time.Minute)}

		if got := RoleOf(dir, 1); got != "admin" {
			t.Fatalf("%q: the last known admin must hold, got %q", body, got)
		}
		if AccountGone(dir, 1) {
			t.Fatalf("%q: not an answer, so not gone", body)
		}
		if n := atomic.LoadInt32(&hits); n != 1 {
			t.Fatalf("%q: a failure waits out the window, got %d requests", body, n)
		}
		if got := RoleOf(dir, 2); got != "user" {
			t.Fatalf("%q: nothing known is user, got %q", body, got)
		}
		server.Close()
	}
}

func TestAccountGoneIsFalseForLiveAccountsAndFailures(t *testing.T) {
	dir, hits, clock, server := roleServer(t, 200, "admin")
	if AccountGone(dir, 1) || RoleOf(dir, 1) != "admin" || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("a live admin: one cached request (hits %d)", atomic.LoadInt32(hits))
	}
	server.Close()
	*clock = clock.Add(10 * time.Minute)
	if AccountGone(dir, 1) || AccountGone(dir, 2) {
		t.Fatal("user-service down: nobody is gone")
	}
}

func TestAGoneAccountIsKeptFiveSeconds(t *testing.T) {
	dir, hits, clock, _ := roleServer(t, 404, "")
	if !AccountGone(dir, 7) || !AccountGone(dir, 7) || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("gone, asked once (hits %d)", atomic.LoadInt32(hits))
	}
	*clock = clock.Add(6 * time.Second)
	AccountGone(dir, 7)
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("asked again after five seconds, got %d", atomic.LoadInt32(hits))
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The box's secret is only sent to an address on this machine.
func TestTheSecretGoesToLoopbackOnly(t *testing.T) {
	var seen []string
	was := roleClient
	roleClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Host+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":200,"data":"admin"}`)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { roleClient = was })

	for _, address := range []string{"http://10.0.0.5:8080", "http://user-service.example:80", "http://[2001:db8::1]:80"} {
		dir := writeAddress(t, address)
		if err := WriteInternalSecret(dir); err != nil {
			t.Fatal(err)
		}
		if got := RoleOf(dir, 1); got != "user" {
			t.Fatalf("%s: user expected, got %q", address, got)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("requests sent off this machine: %v", seen)
	}

	for _, address := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		dir := writeAddress(t, address)
		if err := WriteInternalSecret(dir); err != nil {
			t.Fatal(err)
		}
		if got := RoleOf(dir, 1); got != "admin" {
			t.Fatalf("%s: admin expected, got %q", address, got)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("three loopback requests expected: %v", seen)
	}
}

func TestOnlyTheWordAdminIsAdmin(t *testing.T) {
	for i, word := range []string{"Admin", "", "root", "user"} {
		dir, _, _, _ := roleServer(t, 200, word)
		if got := RoleOf(dir, i); got != "user" {
			t.Fatalf("%q must be user, got %q", word, got)
		}
	}
}

func writeAddress(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UserServiceAddressFilename), []byte(url), 0o600); err != nil {
		t.Fatal(err)
	}
	roleTable = map[int]roleEntry{}
	t.Cleanup(func() { roleNow = time.Now; roleTable = map[int]roleEntry{} })

	return dir
}

func TestSlowUserServiceDoesNotQueueCallers(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	dir := writeAddress(t, server.URL)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := RoleOf(dir, 1); got != "user" {
				t.Errorf("user expected, got %q", got)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("callers queued: %v", d)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("one shared request expected, got %d", n)
	}
}

func TestFailureIsNotRetriedWithinTheFailureWindow(t *testing.T) {
	dir, hits, clock, _ := roleServer(t, 500, "admin")
	RoleOf(dir, 1)
	RoleOf(dir, 1)
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("no retry within the window, got %d", n)
	}
	*clock = clock.Add(3 * time.Second)
	RoleOf(dir, 1)
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("retry after the window, got %d", n)
	}
}

func TestMalformedJSONAndServerErrorGiveStaleOrUser(t *testing.T) {
	for _, status := range []int{200, 500} {
		var body atomic.Value
		body.Store(`{"success":200,"data":"admin"}`)
		code := int32(200)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(int(atomic.LoadInt32(&code)))
			fmt.Fprint(w, body.Load().(string))
		}))
		dir := writeAddress(t, server.URL)
		now := time.Now()
		roleNow = func() time.Time { return now }

		if RoleOf(dir, 1) != "admin" {
			t.Fatal("admin expected")
		}
		body.Store("{not json")
		atomic.StoreInt32(&code, int32(status))
		now = now.Add(10 * time.Second)
		if got := RoleOf(dir, 1); got != "admin" {
			t.Fatalf("status %d: stale admin expected, got %q", status, got)
		}
		if got := RoleOf(dir, 2); got != "user" {
			t.Fatalf("status %d: user expected for unknown, got %q", status, got)
		}
		server.Close()
	}
}

func TestRecoveryAfterOutageRefreshesTheEntry(t *testing.T) {
	code := int32(500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&code)))
		fmt.Fprint(w, `{"success":200,"data":"admin"}`)
	}))
	t.Cleanup(server.Close)
	dir := writeAddress(t, server.URL)
	now := time.Now()
	roleNow = func() time.Time { return now }

	if RoleOf(dir, 1) != "user" {
		t.Fatal("user during the outage")
	}
	atomic.StoreInt32(&code, 200)
	now = now.Add(3 * time.Second)
	if got := RoleOf(dir, 1); got != "admin" {
		t.Fatalf("admin after recovery, got %q", got)
	}
}

func TestAPanicInTheFetchDoesNotBlockLaterCallers(t *testing.T) {
	dir := writeAddress(t, "http://127.0.0.1:1")
	panicked := false
	readUserServiceAddress = func(f string) (string, error) {
		if !panicked {
			panicked = true
			panic("boom")
		}
		return getAddress(f)
	}
	t.Cleanup(func() { readUserServiceAddress = getAddress })

	func() {
		defer func() { _ = recover() }()
		RoleOf(dir, 1)
	}()

	now := time.Now().Add(10 * time.Second)
	roleNow = func() time.Time { return now }
	got := make(chan string, 1)
	go func() { got <- RoleOf(dir, 1) }()
	select {
	case r := <-got:
		if r != "user" {
			t.Fatalf("user expected, got %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a later caller is blocked by the panicked fetch")
	}
}

// A 200 whose envelope is not a success is no answer: the admin known before stays admin.
func TestA200WithoutSuccessIsAFailure(t *testing.T) {
	var body atomic.Value
	body.Store(`{"success":200,"data":"admin"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(server.Close)
	dir := writeAddress(t, server.URL)
	now := time.Now()
	roleNow = func() time.Time { return now }

	if RoleOf(dir, 1) != "admin" {
		t.Fatal("admin expected")
	}
	body.Store(`{"success":500,"data":"user"}`)
	now = now.Add(10 * time.Second)
	if got := RoleOf(dir, 1); got != "admin" {
		t.Fatalf("an envelope that is not a success: %q, want the admin known before", got)
	}
	if _, known := RoleKnown(dir, "2"); known {
		t.Fatal("an envelope that is not a success made a role known")
	}
}

// Account is one answer: the role and whether the account is gone come from the same lookup, so
// an answer that expires between two calls cannot give a gone account a role.
func TestAccountReadsRoleAndGoneFromOneAnswer(t *testing.T) {
	var gone atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"success":%d}`, common_err.USER_NOT_EXIST)
			return
		}
		fmt.Fprint(w, `{"success":200,"data":"user"}`)
	}))
	t.Cleanup(server.Close)
	dir := writeAddress(t, server.URL)
	now := time.Now()
	roleNow = func() time.Time { return now }

	if role, isGone, known := Account(dir, "1"); role != "user" || isGone || !known {
		t.Fatalf("a live user: %q gone=%v known=%v", role, isGone, known)
	}
	gone.Store(true)
	now = now.Add(6 * time.Second)
	if _, isGone, known := Account(dir, "1"); !isGone || !known {
		t.Fatalf("once deleted: gone=%v known=%v", isGone, known)
	}
	for _, id := range []string{"", "0", "-1", "x"} {
		if _, _, known := Account(dir, id); known {
			t.Fatalf("%q is no account", id)
		}
	}
}
