package external

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/common_err"
)

const (
	roleFresh = 5 * time.Second
	roleStale = 5 * time.Minute
)

type roleEntry struct {
	role     string
	rights   []string  // valid names only; read with the role, from the same answer
	gone     bool      // user-service said it has no such account
	at       time.Time // last successful answer; zero when there never was one
	failedAt time.Time // last failed attempt
}

type roleCall struct {
	done  chan struct{}
	entry roleEntry
}

const roleFailureWindow = 2 * time.Second

var (
	roleNow      = time.Now
	roleMu       sync.Mutex // guards the two maps only, never held across a request
	roleTable    = map[int]roleEntry{}
	roleInflight = map[int]*roleCall{}
	roleClient   = &http.Client{Timeout: 2 * time.Second}
)

// RoleOf returns "admin" or "user" for an account. The answer is kept five
// seconds; when user-service does not answer, the last one seen serves for five
// minutes (an admin answer, forever), and a failed attempt is not repeated for two seconds. Anything
// unknown (no answer ever, account gone, any word but "admin") is "user": a
// role is never admin by default. Concurrent callers share one request per
// account. UserService never gives an account number twice; on a database of
// before that, a reused number could keep the old role for up to 5 s.
func RoleOf(runtimePath string, userID int) string {
	return lookupRole(runtimePath, userID).role
}

// RoleKnown is RoleOf for an id given as text, telling a real answer from the
// "user" default: ok is false for an invalid id (empty, zero, negative, not a
// number, overflowing) and when no positive answer is fresh or still allowed to
// stand in. An admin answer, once received, always counts.
func RoleKnown(runtimePath, id string) (role string, ok bool) {
	userID, err := strconv.Atoi(id)
	if err != nil || userID <= 0 {
		return "", false
	}
	e := lookupRole(runtimePath, userID)
	if e.at.IsZero() {
		return "", false
	}

	return e.role, true
}

// Account is RoleKnown and AccountGone from one lookup: a caller that needs both must not ask
// twice, or an answer expiring in between could pair a role with an account that is gone.
func Account(runtimePath, id string) (role string, gone, ok bool) {
	userID, err := strconv.Atoi(id)
	if err != nil || userID <= 0 {
		return "", false, false
	}
	e := lookupRole(runtimePath, userID)
	if e.at.IsZero() {
		return "", false, false
	}

	return e.role, e.gone, true
}

// AccountGone is true when user-service answered that the account does not
// exist, with the same cache as RoleOf. When user-service does not answer, an
// account is not gone unless the last answer, under five minutes old, said so.
func AccountGone(runtimePath string, userID int) bool {
	return lookupRole(runtimePath, userID).gone
}

var validRights = []string{"storage", "vm", "backups", "fleet", "system"}

// ValidRights lists the names a right can have.
func ValidRights() []string { return append([]string(nil), validRights...) }

func validRightsIn(names []string) []string {
	var out []string
	for _, n := range names {
		if slices.Contains(validRights, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// RightsOf is the per-module rights of a user account, from the same cached answer as its role
// (no request of its own). It fails closed: a right is true only while the last successful
// answer is under five seconds old and names it (a name of ValidRights). The role keeps its
// stale grace when user-service is down; the rights do not. The map is empty, never nil, for
// the administrator (who passes by role), a gone account, an unknown role, an invalid id and
// any failure or older answer.
func RightsOf(runtimePath, id string) map[string]bool {
	rights := map[string]bool{}
	userID, err := strconv.Atoi(id)
	if err != nil || userID <= 0 {
		return rights
	}
	e := lookupRole(runtimePath, userID)
	if e.at.IsZero() || e.gone || e.role != "user" || roleNow().Sub(e.at) >= roleFresh {
		return rights
	}
	for _, n := range e.rights {
		rights[n] = true
	}

	return rights
}

func lookupRole(runtimePath string, userID int) (result roleEntry) {
	roleMu.Lock()
	known := roleTable[userID]
	now := roleNow()
	if !known.at.IsZero() && now.Sub(known.at) < roleFresh {
		roleMu.Unlock()
		return known
	}
	if !known.failedAt.IsZero() && now.Sub(known.failedAt) < roleFailureWindow {
		roleMu.Unlock()
		return staleOrUser(known, now)
	}
	if call, ok := roleInflight[userID]; ok {
		roleMu.Unlock()
		<-call.done
		return call.entry
	}
	call := &roleCall{done: make(chan struct{})}
	roleInflight[userID] = call
	roleMu.Unlock()

	// Whatever happens in the fetch, even a panic, the entry is written, the
	// in-flight call removed and the waiters released; a panic counts as a failure.
	role, gone, ok := "", false, false
	var rights []string
	defer func() {
		roleMu.Lock()
		now := roleNow()
		if ok {
			known = roleEntry{role: role, rights: rights, gone: gone, at: now}
			call.entry = known
		} else {
			known.failedAt = now
			call.entry = staleOrUser(known, now)
		}
		roleTable[userID] = known
		delete(roleInflight, userID)
		roleMu.Unlock()
		close(call.done)
		result = call.entry
	}()
	role, rights, gone, ok = fetchRole(runtimePath, userID)

	return roleEntry{}
}

func staleOrUser(e roleEntry, now time.Time) roleEntry {
	// The one admin cannot be deleted or changed: a positive admin answer never expires.
	if e.role == "admin" && !e.gone && !e.at.IsZero() {
		return e
	}
	if !e.at.IsZero() && now.Sub(e.at) < roleStale {
		return e
	}

	return roleEntry{role: "user"}
}

// fetchRole reports ok=false when user-service could not answer. A 404 is an
// answer only when its body is user-service's own word for a missing account
// (gone); any other 404, like an older user-service without this route, is a
// failure. The box's secret is only sent to an address on this machine.
func fetchRole(runtimePath string, userID int) (role string, rights []string, gone, ok bool) {
	address, err := readUserServiceAddress(filepath.Join(runtimePath, UserServiceAddressFilename))
	if err != nil {
		return "", nil, false, false
	}
	roleURL, err := url.JoinPath(address, "/v1/users/role", strconv.Itoa(userID))
	if err != nil {
		return "", nil, false, false
	}
	req, err := http.NewRequest(http.MethodGet, roleURL, nil)
	if err != nil || !isLoopbackHost(req.URL.Hostname()) {
		return "", nil, false, false
	}
	req.Header.Set("Authorization", InternalAuthorization(runtimePath))

	resp, err := roleClient.Do(req)
	if err != nil {
		return "", nil, false, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return "", nil, false, false
	}
	var body struct {
		Success int      `json:"success"`
		Data    string   `json:"data"`
		Rights  []string `json:"rights"` // absent from an older user-service: no rights
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) != nil {
		return "", nil, false, false
	}
	if resp.StatusCode == http.StatusNotFound {
		if body.Success != common_err.USER_NOT_EXIST {
			return "", nil, false, false
		}
		return "user", nil, true, true
	}
	if body.Success != common_err.SUCCESS { // a 200 that is not a success says nothing of the role
		return "", nil, false, false
	}
	if body.Data == "admin" {
		return "admin", nil, false, true
	}

	return "user", validRightsIn(body.Rights), false, true
}
