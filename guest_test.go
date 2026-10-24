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
