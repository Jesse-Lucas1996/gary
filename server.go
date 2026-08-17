package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxWait = 30 * time.Second

type serveOpts struct {
	addr     string
	token    string
	insecure bool
	api      bool
	dbPath   string
	users    *userStore
}

func serveHub(s *Store, o serveOpts) error {
	if err := checkBind(o.addr, o.token, o.insecure); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              o.addr,
		Handler:           hubHandler(s, o),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      maxWait + 30*time.Second,
	}
	announce(o)
	return srv.ListenAndServe()
}

func hubHandler(s *Store, o serveOpts) http.Handler {
	if o.users == nil {
		o.users = &userStore{users: map[string]credential{}}
	}
	mux := http.NewServeMux()
	if o.api {
		registerAPI(mux, s)
	}
	registerDashboard(mux, s, o.dbPath, o.api, !o.users.empty())
	registerLogin(mux, o)
	return authMiddleware(o, mux)
}

func registerLogin(mux *http.ServeMux, o serveOpts) {
	lim := newLimiter()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeLogin(w, "", http.StatusOK)
		case http.MethodPost:
			ip := clientIP(r)
			if locked, left := lim.locked(ip); locked {
				writeLogin(w, fmt.Sprintf("too many attempts — try again in %ds", int(left.Seconds()+1)),
					http.StatusTooManyRequests)
				return
			}
			if err := r.ParseForm(); err != nil {
				writeLogin(w, "bad request", http.StatusBadRequest)
				return
			}
			user := r.PostFormValue("username")
			if !o.users.verify(user, r.PostFormValue("password")) {
				lim.failed(ip)
				writeLogin(w, "wrong username or password", http.StatusUnauthorized)
				return
			}
			lim.ok(ip)
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    newSession(o.users.sessionKey(o.token), user, sessionTTL),
				Path:     "/",
				HttpOnly: true,
				Secure:   isHTTPS(r),
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(sessionTTL / time.Second),
			})
			w.Header().Set("Location", "./")
			w.WriteHeader(http.StatusFound)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: "", Path: "/",
			HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
		w.Header().Set("Location", "./login")
		w.WriteHeader(http.StatusFound)
	})
}

func writeLogin(w http.ResponseWriter, errMsg string, status int) {
	page := loginHTML
	if errMsg != "" {
		page = bytes.Replace(page, []byte("<!--ERROR-->"),
			[]byte(`<div class="error">`+html.EscapeString(errMsg)+`</div>`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(page)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func checkBind(addr, token string, insecure bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad --addr %q: %w", addr, err)
	}
	public, why := isPublicBind(host)
	if public && !insecure {
		return fmt.Errorf(`refusing to bind %s: %s

gary's token can start processes on your machines, so don't put it on the open
internet. Pick one:
  --addr 127.0.0.1:PORT    and reverse-proxy it with TLS (nginx/caddy)
  --addr <tailscale-ip>:PORT   or a LAN/VPN address
  --insecure               if you really mean it (you accept the exposure)`, addr, why)
	}
	if !isLoopback(host) && token == "" {
		return errors.New("a token is required when not bound to loopback:\n" +
			"  gary token new    # writes ~/.config/gary/token\n" +
			"  gary serve --token <secret>")
	}
	return nil
}

func isPublicBind(host string) (bool, string) {
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return true, "that listens on every interface, including any public one"
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true, fmt.Sprintf("cannot resolve %q to check whether it is private", host)
	}
	for _, ip := range ips {
		if !isPrivateIP(ip) {
			return true, fmt.Sprintf("%s is a public address", ip)
		}
	}
	return false, ""
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	cgnat := net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	return cgnat.Contains(ip)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func authMiddleware(o serveOpts, next http.Handler) http.Handler {
	open := o.token == "" && o.users.empty()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || r.URL.Path == "/logout" {
			next.ServeHTTP(w, r)
			return
		}
		if open || authorized(o, r) {
			next.ServeHTTP(w, r)
			return
		}
		if !o.users.empty() && wantsHTML(r) {
			w.Header().Set("Location", loginTarget(r.URL.Path))
			w.WriteHeader(http.StatusFound)
			return
		}
		writeErr(w, http.StatusUnauthorized, ErrUnauthorized)
	})
}

func authorized(o serveOpts, r *http.Request) bool {
	if o.token != "" && tokenOK(o.token, bearerToken(r)) {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := validSession(o.users.sessionKey(o.token), c.Value); ok {
			return true
		}
	}
	return false
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func loginTarget(p string) string {
	if p == "" || strings.HasSuffix(p, "/") {
		return "login"
	}
	return path.Join(strings.Repeat("../", strings.Count(strings.Trim(p, "/"), "/")), "login")
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func tokenOK(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func registerAPI(mux *http.ServeMux, s *Store) {
	handle(mux, "/v1/register", func(r *http.Request) (any, error) {
		var in struct{ Name, Description, Node string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{}, s.RegisterOn(in.Name, in.Description, in.Node)
	})
	handle(mux, "/v1/unregister", func(r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{}, s.Unregister(in.Name)
	})
	handle(mux, "/v1/agents", func(r *http.Request) (any, error) {
		return nonNil(s.List())
	})

	handle(mux, "/v1/send", func(r *http.Request) (any, error) {
		var in struct{ From, To, Body string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		id, err := s.Send(in.From, in.To, in.Body)
		if err != nil {
			return nil, err
		}
		return map[string]int64{"id": id}, nil
	})
	handle(mux, "/v1/inbox", func(r *http.Request) (any, error) {
		return nonNil(s.Inbox(r.URL.Query().Get("agent")))
	})
	handle(mux, "/v1/recv", func(r *http.Request) (any, error) {
		var in struct {
			Agent  string `json:"agent"`
			WaitMS int64  `json:"wait_ms"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return pollUntil(r.Context(), waitFor(in.WaitMS), func() (*Message, error) { return s.Recv(in.Agent) })
	})
	handle(mux, "/v1/recent", func(r *http.Request) (any, error) {
		return nonNil(s.Recent(intParam(r, "limit", 200)))
	})

	handle(mux, "/v1/nodes", func(r *http.Request) (any, error) {
		return nonNil(s.Nodes())
	})
	handle(mux, "/v1/nodes/heartbeat", func(r *http.Request) (any, error) {
		var in struct{ Name, Version string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{}, s.Heartbeat(in.Name, in.Version)
	})
	handle(mux, "/v1/nodes/claim", func(r *http.Request) (any, error) {
		var in struct {
			Node   string `json:"node"`
			WaitMS int64  `json:"wait_ms"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return pollUntil(r.Context(), waitFor(in.WaitMS), func() (*Spawn, error) { return s.ClaimSpawn(in.Node) })
	})
	handle(mux, "/v1/spawn", func(r *http.Request) (any, error) {
		var spec SpawnSpec
		if err := decode(r, &spec); err != nil {
			return nil, err
		}
		id, err := s.Spawn(spec)
		if err != nil {
			return nil, err
		}
		return map[string]int64{"id": id}, nil
	})
	handle(mux, "/v1/spawns", func(r *http.Request) (any, error) {
		return nonNil(s.Spawns(intParam(r, "limit", 100)))
	})
	handle(mux, "/v1/spawns/get", func(r *http.Request) (any, error) {
		st, err := s.SpawnStatus(int64(intParam(r, "id", 0)))
		if err != nil {
			return nil, err
		}
		return map[string]string{"status": st}, nil
	})
	handle(mux, "/v1/spawns/status", func(r *http.Request) (any, error) {
		var in struct {
			ID        int64  `json:"id"`
			Status    string `json:"status"`
			SessionID string `json:"session_id"`
			PID       int    `json:"pid"`
			Error     string `json:"error"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{}, s.UpdateSpawn(in.ID, in.Status, in.SessionID, in.PID, in.Error)
	})
	handle(mux, "/v1/spawns/stop", func(r *http.Request) (any, error) {
		var in struct{ Agent string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{}, s.StopSpawn(in.Agent)
	})
}

func registerDashboard(mux *http.ServeMux, s *Store, dbPath string, api, login bool) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		agents, err := s.List()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		msgs, err := s.Recent(200)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		nodes, err := s.Nodes()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		spawns, err := s.Spawns(100)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"agents": nilAsEmpty(agents), "messages": nilAsEmpty(msgs),
			"nodes": nilAsEmpty(nodes), "spawns": nilAsEmpty(spawns),
			"db_path": dbPath, "api": api, "login": login, "stale_after": int64(3 * heartbeatEvery / time.Second),
		})
	})
}

type ok struct {
	OK bool `json:"ok"`
}

func handle(mux *http.ServeMux, path string, fn func(*http.Request) (any, error)) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrGuard), errors.Is(err, ErrUnregistered):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, context.Canceled):
		return 499
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	code := ""
	switch {
	case errors.Is(err, ErrGuard):
		code = "guard"
	case errors.Is(err, ErrUnregistered):
		code = "unregistered"
	case errors.Is(err, ErrUnauthorized):
		code = "unauthorized"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errResponse{Error: err.Error(), Code: code})
}

func decode(r *http.Request, v any) error {
	if r.Method != http.MethodPost {
		return fmt.Errorf("%s requires POST", r.URL.Path)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func waitFor(ms int64) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	if d < 0 {
		return 0
	}
	if d > maxWait {
		return maxWait
	}
	return d
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func nonNil[T any](v []T, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return nilAsEmpty(v), nil
}

func nilAsEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func announce(o serveOpts) {
	scheme := "http://"
	host := o.addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	fmt.Printf("gary hub on %s%s (db: %s)\n", scheme, host, o.dbPath)
	if !o.api {
		fmt.Println("dashboard only — no API. use `gary serve` to accept remote agents.")
		return
	}
	fmt.Println("\nconnect from another machine:")
	fmt.Printf("  export GARY_URL=%s%s\n", scheme, host)
	if o.token != "" {
		fmt.Println("  export GARY_TOKEN=<the token>")
	}
	if o.insecure {
		fmt.Fprintln(os.Stderr, "\nwarning: --insecure — this hub is bound to a public interface.")
	}
}
