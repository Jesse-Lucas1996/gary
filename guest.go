package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Guest accounts: a dashboard login that sees only the channels it is a member
// of, and can do nothing but post to them. Hiding other rooms in the page is not
// enough on its own — the session cookie authorises the whole API — so the gate
// sits in front of every route and lets a guest through only to the few the
// dashboard needs, checking who they post as and where.

// guestGate passes everything for full accounts and the bearer token, and
// restricts a guest session to the dashboard, its state, and posting as itself.
func guestGate(s *Store, o serveOpts, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := sessionUser(o, r)
		if user == "" || !o.users.isGuest(user) || (o.token != "" && tokenOK(o.token, bearerToken(r))) {
			next.ServeHTTP(w, r)
			return
		}
		if _, static := pwaFiles[r.URL.Path]; static {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/", "/login", "/logout", "/api/state":
			next.ServeHTTP(w, r)
			return
		case "/v1/register", "/v1/post":
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			// Same field names and decoding rules as the handlers, so the gate and
			// the handler can't read the body differently.
			var in struct{ Name, Node, From, Channel string }
			json.Unmarshal(body, &in)
			var allowed bool
			if r.URL.Path == "/v1/register" {
				allowed = in.Name == user && in.Node == ""
			} else {
				chans, err := guestChannels(s, user)
				allowed = err == nil && in.From == user && chans[in.Channel]
			}
			if allowed {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, http.StatusForbidden, ErrUnauthorized)
	})
}

// guestChannels is the set of channels a guest belongs to.
func guestChannels(s *Store, user string) (map[string]bool, error) {
	all, err := s.Channels()
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for _, c := range all {
		for _, m := range c.Members {
			if m == user {
				in[c.Name] = true
			}
		}
	}
	return in, nil
}

// guestView trims dashboard state to what a guest may see: their channels, the
// mail posted to them or sent to/from the guest directly, and the agents (and
// those agents' spawns) that share a channel with them.
func guestView(user string, channels []Channel, msgs []Message, agents []Agent, spawns []Spawn) ([]Channel, []Message, []Agent, []Spawn) {
	var vc []Channel
	members := map[string]bool{user: true}
	for _, c := range channels {
		for _, m := range c.Members {
			if m == user {
				vc = append(vc, c)
				for _, mm := range c.Members {
					members[mm] = true
				}
				break
			}
		}
	}
	visible := map[string]bool{}
	for _, c := range vc {
		visible[c.Name] = true
	}
	var vm []Message
	for _, m := range msgs {
		if visible[m.Channel] || (m.Channel == "" && (m.To == user || m.From == user)) {
			vm = append(vm, m)
		}
	}
	var va []Agent
	for _, a := range agents {
		if members[a.Name] {
			va = append(va, a)
		}
	}
	// An agent the guest shares one room with may also work in rooms the guest
	// can't see. Its step and last result could come from those, so a guest gets
	// only the timing for it — enough to tell working from idle.
	private := map[string]bool{}
	for _, c := range channels {
		if !visible[c.Name] {
			for _, m := range c.Members {
				private[m] = true
			}
		}
	}
	var vs []Spawn
	for _, sp := range spawns {
		if members[sp.Agent] {
			sp.Prompt = "" // the operator's instructions to the agent, not the guest's business
			if private[sp.Agent] {
				sp.Step, sp.LastResult, sp.TurnFrom = "", "", ""
			}
			vs = append(vs, sp)
		}
	}
	return vc, vm, va, vs
}
