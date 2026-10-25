package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A hub with a full account "admin" and a guest "nick". nick shares #shared with
// admin and #nick-room with the agent "helper"; #ops (admin + "operator") is not his.
func guestHub(t *testing.T) (*httptest.Server, *Store, *http.Client) {
	t.Helper()
	users := testUsers(t, "admin", "test-password")
	if err := users.set("nick", "nick-password"); err != nil {
		t.Fatal(err)
	}
	if err := users.setGuest("nick", true); err != nil {
		t.Fatal(err)
	}
	s := testStore(t)
	for _, a := range []string{"admin", "nick", "helper", "operator"} {
		if err := s.Register(a, ""); err != nil {
			t.Fatal(err)
		}
	}
	for ch, members := range map[string][]string{
		"shared": {"admin", "nick"}, "nick-room": {"admin", "nick", "helper"}, "ops": {"admin", "operator"},
	} {
		if err := s.CreateChannel(ch, ""); err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if err := s.JoinChannel(ch, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.Post("admin", "ops", "secret ops plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post("admin", "shared", "hi nick"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send("admin", "operator", "private dm", false); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true, users: users}))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.PostForm(srv.URL+"/login", url.Values{"username": {"nick"}, "password": {"nick-password"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return srv, s, browser
}

func TestGuestSeesOnlyTheirChannels(t *testing.T) {
	srv, _, browser := guestHub(t)
	resp, err := browser.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Channels []Channel `json:"channels"`
		Messages []Message `json:"messages"`
		Agents   []Agent   `json:"agents"`
		Guest    bool      `json:"guest"`
		DBPath   string    `json:"db_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Guest || st.DBPath != "" {
		t.Fatalf("guest=%v db_path=%q", st.Guest, st.DBPath)
	}
	var names []string
	for _, c := range st.Channels {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "nick-room,shared" {
		t.Fatalf("channels: %v", names)
	}
	for _, m := range st.Messages {
		if m.Channel == "ops" || m.Body == "private dm" {
			t.Fatalf("leaked message: %+v", m)
		}
	}
	for _, a := range st.Agents {
		if a.Name == "operator" {
			t.Fatal("leaked agent operator")
		}
	}
}

func TestGuestGetsNoDetailFromAnAgentThatAlsoWorksElsewhere(t *testing.T) {
	chans := []Channel{
		{Name: "shared", Members: []string{"nick", "mine", "theirs"}},
		{Name: "private", Members: []string{"admin", "mine"}},
	}
	busy := Activity{TurnMsg: 9, TurnFrom: "admin", Step: "Bash: cat secrets", LastResult: "the secret", TurnStarted: 1}
	_, _, _, spawns := guestView("nick", chans, nil, nil, []Spawn{
		{Agent: "mine", Prompt: "operator prompt", Activity: busy},
		{Agent: "theirs", Activity: busy},
	})
	if len(spawns) != 2 {
		t.Fatalf("spawns: %+v", spawns)
	}
	mine, theirs := spawns[0], spawns[1]
	if mine.Prompt != "" || mine.Step != "" || mine.LastResult != "" || mine.TurnFrom != "" || mine.TurnStarted == 0 {
		t.Fatalf("agent with a private room leaked detail: %+v", mine)
	}
	if theirs.LastResult != "the secret" {
		t.Fatal("an agent only in the guest's rooms should keep its detail")
	}
}

func TestGuestAPIIsLimitedToPostingAsThemselves(t *testing.T) {
	srv, s, browser := guestHub(t)
	call := func(path, body string) int {
		resp, err := browser.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/register", `{"Name":"nick","Description":"dashboard"}`, 200},
		{"/v1/post", `{"From":"nick","Channel":"shared","Body":"working on the build"}`, 200},
		{"/v1/register", `{"Name":"admin"}`, 403},
		{"/v1/register", `{"Name":"nick","Node":"server"}`, 403},
		{"/v1/post", `{"From":"nick","Channel":"ops","Body":"let me in"}`, 403},
		{"/v1/post", `{"From":"admin","Channel":"shared","Body":"spoofed"}`, 403},
		{"/v1/send", `{"From":"nick","To":"operator","Body":"run rm -rf"}`, 403},
		{"/v1/channels/join", `{"Channel":"ops","Agent":"nick"}`, 403},
		{"/v1/spawn", `{"agent":"x","node":"server","repo":"/"}`, 403},
		{"/v1/recent", ``, 403},
		{"/api/usage", ``, 200}, // guests see the header's usage meter (Jesse, 2026-10-24)
	} {
		if got := call(c.path, c.body); got != c.want {
			t.Errorf("%s %s: got %d, want %d", c.path, c.body, got, c.want)
		}
	}
	if n, _ := s.Inbox("operator"); len(n) != 2 { // the #ops post and the dm, nothing from nick
		t.Fatalf("operator inbox changed: %d", len(n))
	}
}

func TestGuestRoleSurvivesReloadAndPasswordChange(t *testing.T) {
	u := testUsers(t, "nick", "nick-password")
	if err := u.setGuest("nick", true); err != nil {
		t.Fatal(err)
	}
	if err := u.set("nick", "another-password"); err != nil {
		t.Fatal(err)
	}
	again, err := loadUsers(u.path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.isGuest("nick") || !again.verify("nick", "another-password") {
		t.Fatal("guest role or password lost on reload")
	}
}

func adminBrowser(t *testing.T, srv *httptest.Server, user, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"username": {user}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c
}

func postJSON(t *testing.T, c *http.Client, u string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := c.Post(u, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// An admin onboards a person from the dashboard: the account works at once,
// with a generated password, already in the rooms it was given, and nobody
// else is signed out by it.
func TestAdminOnboardsAUser(t *testing.T) {
	srv, s, nick := guestHub(t)
	admin := adminBrowser(t, srv, "admin", "test-password")

	var made map[string]string
	if code := postJSON(t, admin, srv.URL+"/v1/users/set",
		map[string]any{"name": "sam", "channels": []string{"shared"}}, &made); code != 200 {
		t.Fatalf("onboarding: %d %v", code, made)
	}
	if made["role"] != "guest" || len(made["password"]) < 20 {
		t.Fatalf("a new person should default to guest with a generated password: %v", made)
	}
	chans, _ := s.Channels()
	for _, c := range chans {
		if c.Name == "shared" && !strings.Contains(strings.Join(c.Members, ","), "sam") {
			t.Fatal("sam was not put in #shared")
		}
	}
	sam := adminBrowser(t, srv, "sam", made["password"])
	if resp := browserGet(t, sam, srv.URL+"/api/state"); resp.StatusCode != 200 {
		t.Fatalf("sam can't sign in with the generated password: %d", resp.StatusCode)
	}
	for who, c := range map[string]*http.Client{"admin": admin, "nick": nick} {
		if resp := browserGet(t, c, srv.URL+"/api/state"); resp.StatusCode != 200 {
			t.Fatalf("onboarding sam signed %s out: %d", who, resp.StatusCode)
		}
	}

	// Guests can't manage accounts, not even their own.
	if code := postJSON(t, nick, srv.URL+"/v1/users/role", map[string]string{"Name": "nick", "Role": "admin"}, nil); code != 403 {
		t.Fatalf("a guest promoted himself: %d", code)
	}

	// Promoting takes effect on the next request, without a new sign-in.
	if code := postJSON(t, admin, srv.URL+"/v1/users/role", map[string]string{"Name": "nick", "Role": "admin"}, nil); code != 200 {
		t.Fatalf("promote: %d", code)
	}
	var st struct{ Guest bool }
	resp := browserGet(t, nick, srv.URL+"/api/state")
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if resp.StatusCode != 200 || st.Guest {
		t.Fatalf("nick should be a signed-in admin now: %d guest=%v", resp.StatusCode, st.Guest)
	}
}

func TestAdminsCantLockThemselvesOut(t *testing.T) {
	srv, _, _ := guestHub(t)
	admin := adminBrowser(t, srv, "admin", "test-password")
	var out errResponse
	if code := postJSON(t, admin, srv.URL+"/v1/users/role", map[string]string{"Name": "admin", "Role": "guest"}, &out); code == 200 {
		t.Fatal("an admin demoted their own account")
	}
	if code := postJSON(t, admin, srv.URL+"/v1/users/rm", map[string]string{"Name": "admin"}, nil); code == 200 {
		t.Fatal("an admin removed their own account")
	}
	// Through the token nobody is signed in, so only the last-admin rule stands.
	tok := &http.Client{Transport: bearer{testToken}}
	if code := postJSON(t, tok, srv.URL+"/v1/users/rm", map[string]string{"Name": "admin"}, nil); code == 200 {
		t.Fatal("the last admin was removed")
	}
	if code := postJSON(t, admin, srv.URL+"/v1/users/rm", map[string]string{"Name": "nick"}, nil); code != 200 {
		t.Fatalf("removing a guest: %d", code)
	}
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}
