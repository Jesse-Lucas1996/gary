package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iters   = 600000
	pbkdf2KeyLen  = 32
	saltLen       = 16
	sessionTTL    = 30 * 24 * time.Hour
	sessionCookie = "gary_session"
)

type credential struct {
	iters int
	salt  []byte
	hash  []byte
}

type userStore struct {
	path  string
	mu    sync.RWMutex
	users map[string]credential
	raw   []byte
}

func UsersPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gary", "users"), nil
}

func loadUsers(path string) (*userStore, error) {
	s := &userStore{path: path, users: map[string]credential{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "gary: warning: %s is mode %04o; run `chmod 600 %s`\n",
			path, info.Mode().Perm(), path)
	}
	s.raw = raw
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, enc, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected name:hash", path, n+1)
		}
		c, err := parseCredential(enc)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n+1, err)
		}
		s.users[name] = c
	}
	return s, nil
}

func parseCredential(enc string) (credential, error) {
	parts := strings.Split(enc, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return credential{}, fmt.Errorf("unsupported hash format")
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters < 1 {
		return credential{}, fmt.Errorf("bad iteration count")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return credential{}, fmt.Errorf("bad salt")
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return credential{}, fmt.Errorf("bad hash")
	}
	return credential{iters: iters, salt: salt, hash: hash}, nil
}

func encodeCredential(c credential) string {
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", c.iters,
		base64.RawStdEncoding.EncodeToString(c.salt),
		base64.RawStdEncoding.EncodeToString(c.hash))
}

func derive(password string, salt []byte, iters int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iters, pbkdf2KeyLen)
}

func (s *userStore) verify(name, password string) bool {
	s.mu.RLock()
	c, ok := s.users[name]
	s.mu.RUnlock()
	if !ok {
		_, _ = derive(password, make([]byte, saltLen), pbkdf2Iters)
		return false
	}
	got, err := derive(password, c.salt, c.iters)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, c.hash) == 1
}

func (s *userStore) set(name, password string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, ":\n") {
		return fmt.Errorf("invalid user name %q", name)
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := derive(password, salt, pbkdf2Iters)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.users[name] = credential{iters: pbkdf2Iters, salt: salt, hash: hash}
	s.mu.Unlock()
	return s.save()
}

func (s *userStore) remove(name string) error {
	s.mu.Lock()
	_, ok := s.users[name]
	delete(s.users, name)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such user %q", name)
	}
	return s.save()
}

func (s *userStore) names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.users))
	for n := range s.users {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *userStore) empty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users) == 0
}

func (s *userStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, n := range s.names() {
		s.mu.RLock()
		c := s.users[n]
		s.mu.RUnlock()
		fmt.Fprintf(&b, "%s:%s\n", n, encodeCredential(c))
	}
	raw := []byte(b.String())
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.raw = raw
	return nil
}

func (s *userStore) sessionKey(token string) []byte {
	s.mu.RLock()
	raw := s.raw
	s.mu.RUnlock()
	sum := sha256.Sum256(append([]byte("gary-session\x00"+token+"\x00"), raw...))
	return sum[:]
}

func newSession(key []byte, user string, ttl time.Duration) string {
	payload := fmt.Sprintf("%s|%d", user, time.Now().Add(ttl).Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validSession(key []byte, value string) (string, bool) {
	encPayload, encMAC, ok := strings.Cut(value, ".")
	if !ok {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encMAC)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	user, exp, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return "", false
	}
	return user, true
}

type limiter struct {
	mu   sync.Mutex
	fail map[string]*attempt
}

type attempt struct {
	count int
	until time.Time
}

const (
	maxFailures = 5
	lockout     = time.Minute
)

func newLimiter() *limiter { return &limiter{fail: map[string]*attempt{}} }

func (l *limiter) locked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.fail[ip]
	if a == nil || time.Now().After(a.until) {
		return false, 0
	}
	if a.count < maxFailures {
		return false, 0
	}
	return true, time.Until(a.until)
}

func (l *limiter) failed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.fail[ip]
	if a == nil || time.Now().After(a.until) {
		a = &attempt{}
		l.fail[ip] = a
	}
	a.count++
	a.until = time.Now().Add(lockout)
}

func (l *limiter) ok(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, ip)
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if first, _, ok := strings.Cut(f, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(f)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var stdin struct {
	once sync.Once
	r    *bufio.Reader
}

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	if interactive {
		if err := stty("-echo"); err == nil {
			defer func() {
				stty("echo")
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	stdin.once.Do(func() { stdin.r = bufio.NewReader(os.Stdin) })
	line, err := stdin.r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func stty(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
