package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testUsers(t *testing.T, name, password string) *userStore {
	t.Helper()
	u, err := loadUsers(filepath.Join(t.TempDir(), "users"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		if err := u.set(name, password); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func TestPasswordHashRoundTrip(t *testing.T) {
	u := testUsers(t, "admin", "correct horse battery")
	if !u.verify("admin", "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if u.verify("admin", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if u.verify("nobody", "correct horse battery") {
		t.Fatal("unknown user accepted")
	}
}

func TestPasswordNeverStoredInPlaintext(t *testing.T) {
	const pw = "hunter2-test-pw"
	path := filepath.Join(t.TempDir(), "users")
	u, err := loadUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.set("admin", pw); err != nil {
		t.Fatal(err)
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawBytes)
	if strings.Contains(raw, pw) {
		t.Fatal("password written to disk in plaintext")
	}
	if !strings.HasPrefix(raw, "admin:pbkdf2-sha256$") {
		t.Fatalf("unexpected on-disk format: %q", raw)
	}
	reloaded, err := loadUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.verify("admin", pw) {
		t.Fatal("hash did not survive a reload")
	}
}

func TestShortPasswordRejected(t *testing.T) {
	u := testUsers(t, "", "")
	if err := u.set("admin", "short"); err == nil {
		t.Fatal("expected a minimum length to be enforced")
	}
}

func TestSessionCookieSigning(t *testing.T) {
	key := []byte("a-test-key-of-some-length-------")
	tok := newSession(key, "admin", time.Hour)
	if u, ok := validSession(key, tok); !ok || u != "admin" {
		t.Fatalf("valid session rejected: %q %v", u, ok)
	}
	if _, ok := validSession([]byte("different-key-------------------"), tok); ok {
		t.Fatal("session accepted under the wrong key")
	}
	if _, ok := validSession(key, tok+"x"); ok {
		t.Fatal("tampered session accepted")
	}
	if _, ok := validSession(key, newSession(key, "admin", -time.Minute)); ok {
		t.Fatal("expired session accepted")
	}
}

func TestChangingPasswordInvalidatesSessions(t *testing.T) {
	u := testUsers(t, "admin", "first password")
	tok := newSession(u.sessionKey("tok", "admin"), "admin", time.Hour)
	if _, ok := u.session("tok", tok); !ok {
		t.Fatal("session should be valid before the change")
	}
	if err := u.set("admin", "second password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.session("tok", tok); ok {
		t.Fatal("old session survived a password change")
	}
}

// Onboarding someone, or resetting their password, must not sign anyone else out.
func TestSessionsArePerAccount(t *testing.T) {
	u := testUsers(t, "admin", "admin password")
	if err := u.set("nick", "nick password"); err != nil {
		t.Fatal(err)
	}
	admin := newSession(u.sessionKey("tok", "admin"), "admin", time.Hour)
	nick := newSession(u.sessionKey("tok", "nick"), "nick", time.Hour)
	if err := u.set("nick", "new nick password"); err != nil {
		t.Fatal(err)
	}
	if err := u.set("newcomer", "newcomer password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.session("tok", admin); !ok {
		t.Fatal("another account's password change signed admin out")
	}
	if _, ok := u.session("tok", nick); ok {
		t.Fatal("nick's old session survived his password change")
	}
	if err := u.setGuest("admin", true); err != nil {
		t.Fatal(err)
	}
	if name, ok := u.session("tok", admin); !ok || name != "admin" {
		t.Fatal("a role change should apply without ending the session")
	}
	if err := u.remove("admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.session("tok", admin); ok {
		t.Fatal("a removed account's session still works")
	}
	if _, ok := u.session("other-token", newSession(u.sessionKey("tok", "nick"), "nick", time.Hour)); ok {
		t.Fatal("a session outlived a token rotation")
	}
}

// `gary user` writes the file from another process; the hub must notice.
func TestUsersFileEditedElsewhereIsPickedUp(t *testing.T) {
	hub := testUsers(t, "admin", "admin password")
	cli, err := loadUsers(hub.path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a distinct mtime even on coarse filesystems
	if err := cli.set("nick", "nick password"); err != nil {
		t.Fatal(err)
	}
	if err := cli.setGuest("nick", true); err != nil {
		t.Fatal(err)
	}
	hub.refresh()
	if !hub.verify("nick", "nick password") || !hub.isGuest("nick") {
		t.Fatal("hub did not pick up the outside edit")
	}
	if err := os.WriteFile(hub.path, []byte("garbage line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub.refresh()
	if !hub.verify("admin", "admin password") {
		t.Fatal("a broken users file should keep the accounts already loaded")
	}
}

func browserGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func loginHub(t *testing.T, users *userStore) (*httptest.Server, *http.Client) {
	t.Helper()
	s := testStore(t)
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true, users: users}))
	t.Cleanup(srv.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv, &http.Client{Jar: jar}
}

func TestLoginFlow(t *testing.T) {
	users := testUsers(t, "admin", "test-password")
	srv, browser := loginHub(t, users)

	resp := browserGet(t, browser, srv.URL+"/")
	if !strings.HasSuffix(resp.Request.URL.Path, "/login") {
		t.Fatalf("unauthenticated browser should land on the login page, got %q", resp.Request.URL.Path)
	}

	resp, err := browser.PostForm(srv.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", resp.StatusCode)
	}

	resp, err = browser.PostForm(srv.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("after login expected the dashboard, got %d at %q", resp.StatusCode, resp.Request.URL.Path)
	}

	resp, err = browser.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session should reach /api/state, got %d", resp.StatusCode)
	}

	browserGet(t, browser, srv.URL+"/logout")

	resp, err = browser.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("logout did not end the session")
	}
}

func TestAPITokenStillWorksAlongsideLogin(t *testing.T) {
	users := testUsers(t, "admin", "test-password")
	s := testStore(t)
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true, users: users}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, testToken).List(); err != nil {
		t.Fatalf("bearer token must keep working for agents and nodes: %v", err)
	}
	if _, err := NewClient(srv.URL, "wrong").List(); err == nil {
		t.Fatal("wrong bearer token accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	users := testUsers(t, "admin", "test-password")
	srv, browser := loginHub(t, users)
	var last int
	for i := 0; i < maxFailures+1; i++ {
		resp, err := browser.PostForm(srv.URL+"/login", url.Values{
			"username": {"admin"}, "password": {"nope"},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected lockout after %d failures, got %d", maxFailures, last)
	}
	resp, err := browser.PostForm(srv.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("lockout must hold even for the correct password")
	}
}

func TestLoginWorksUnderSubPath(t *testing.T) {
	users := testUsers(t, "admin", "test-password")
	s := testStore(t)
	outer := http.NewServeMux()
	outer.Handle("/gary/", http.StripPrefix("/gary",
		hubHandler(s, serveOpts{token: testToken, api: true, users: users})))
	srv := httptest.NewServer(outer)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}

	resp := browserGet(t, browser, srv.URL+"/gary/")
	if resp.Request.URL.Path != "/gary/login" {
		t.Fatalf("login redirect left the sub-path: %q", resp.Request.URL.Path)
	}

	resp, err := browser.PostForm(srv.URL+"/gary/login", url.Values{
		"username": {"admin"}, "password": {"test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/gary/" || resp.StatusCode != http.StatusOK {
		t.Fatalf("post-login landed on %q (%d), want /gary/ 200", resp.Request.URL.Path, resp.StatusCode)
	}

	c := NewClient(srv.URL+"/gary", testToken)
	if err := c.RegisterOn("a", "", ""); err != nil {
		t.Fatalf("API under sub-path: %v", err)
	}
}

func TestPWAFilesAreServedWithoutASession(t *testing.T) {
	users := testUsers(t, "admin", "test-password")
	s := testStore(t)
	outer := http.NewServeMux()
	outer.Handle("/gary/", http.StripPrefix("/gary",
		hubHandler(s, serveOpts{token: testToken, api: true, users: users})))
	srv := httptest.NewServer(outer)
	defer srv.Close()

	for p, ctype := range pwaFiles {
		resp, err := http.Get(srv.URL + "/gary" + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ctype {
			t.Errorf("%s: %d %q, want 200 %q", p, resp.StatusCode, resp.Header.Get("Content-Type"), ctype)
		}
	}
	// The exemption is those files only: data stays behind the login.
	resp, err := http.Get(srv.URL + "/gary/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api/state without a session: %d, want 401", resp.StatusCode)
	}
}
