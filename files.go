package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Pictures on messages. A file is uploaded to the hub once and gets an
// unguessable id; a message carries it as a marker line in its body,
//
//	[image: name.png — gary file get <id>]
//
// so every reader that only knows bodies (the node feeding a turn, `recv`, old
// dashboards) still sees it and can fetch it, and an agent opens the picture by
// running exactly that command. The dashboard renders the marker inline.

const maxFileBytes = 10 << 20

// Only raster images: anything the dashboard would show inline must not be able
// to carry script (no SVG, no HTML).
var imageTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

type FileRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mime      string `json:"mime"`
	Size      int    `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

var ErrNotFound = errors.New("not found")

const filesSchema = `
CREATE TABLE IF NOT EXISTS files (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    mime       TEXT NOT NULL,
    size       INTEGER NOT NULL,
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);`

var markerRe = regexp.MustCompile(`\[image: [^\]\n]*? — gary file get ([0-9a-f]{32})\]`)

// Marker is the body line that references a file.
func Marker(f FileRef) string {
	name := strings.NewReplacer("]", ")", "\n", " ").Replace(f.Name)
	return fmt.Sprintf("[image: %s — gary file get %s]", name, f.ID)
}

// checkImage sniffs the bytes (never trusts the declared type) and returns the mime.
func checkImage(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%w: empty file", ErrGuard)
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("%w: file is %d bytes; the limit is %d", ErrGuard, len(data), maxFileBytes)
	}
	mime := http.DetectContentType(data)
	if _, ok := imageTypes[mime]; !ok {
		return "", fmt.Errorf("%w: only PNG, JPEG, GIF or WebP pictures can be attached (got %s)", ErrGuard, mime)
	}
	return mime, nil
}

func newFileID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) ensureFiles() error {
	_, err := s.db.Exec(filesSchema)
	return err
}

func (s *Store) PutFile(name string, data []byte) (FileRef, error) {
	mime, err := checkImage(data)
	if err != nil {
		return FileRef{}, err
	}
	if err := s.ensureFiles(); err != nil {
		return FileRef{}, err
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		name = "picture" + imageTypes[mime]
	}
	f := FileRef{ID: newFileID(), Name: name, Mime: mime, Size: len(data), CreatedAt: time.Now().Unix()}
	_, err = s.db.Exec(`INSERT INTO files(id, name, mime, size, data, created_at) VALUES(?,?,?,?,?,?)`,
		f.ID, f.Name, f.Mime, f.Size, data, f.CreatedAt)
	if err != nil {
		return FileRef{}, fmt.Errorf("put file: %w", err)
	}
	return f, nil
}

func (s *Store) GetFile(id string) (FileRef, []byte, error) {
	if err := s.ensureFiles(); err != nil {
		return FileRef{}, nil, err
	}
	var f FileRef
	var data []byte
	err := s.db.QueryRow(`SELECT id, name, mime, size, data, created_at FROM files WHERE id=?`, id).
		Scan(&f.ID, &f.Name, &f.Mime, &f.Size, &data, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FileRef{}, nil, fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	if err != nil {
		return FileRef{}, nil, fmt.Errorf("get file: %w", err)
	}
	return f, data, nil
}

// ---- HTTP ----

func registerFiles(mux *http.ServeMux, s *Store) {
	// POST /v1/files  raw bytes, ?name=<filename>  -> FileRef
	mux.HandleFunc("/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, errors.New("POST a file"))
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxFileBytes+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		f, err := s.PutFile(r.URL.Query().Get("name"), data)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f)
	})
	// GET /v1/files/<id>  -> the picture
	mux.HandleFunc("/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/files/")
		f, data, err := s.GetFile(id)
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		w.Header().Set("Content-Type", f.Mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", f.Name))
		w.Write(data)
	})
}

// guestMaySee: a guest can fetch a file only if it is referenced by a message
// the guest can already read.
func guestMaySee(s *Store, user, id string) bool {
	agents, err := s.List()
	if err != nil {
		return false
	}
	msgs, err := s.Recent(1000)
	if err != nil {
		return false
	}
	channels, err := s.Channels()
	if err != nil {
		return false
	}
	_, vm, _, _ := guestView(user, channels, msgs, agents, nil)
	for _, m := range vm {
		if strings.Contains(m.Body, id) && markerRe.MatchString(m.Body) {
			return true
		}
	}
	return false
}

// ---- client ----

func (c *Client) PutFile(name string, data []byte) (FileRef, error) {
	var f FileRef
	req, err := http.NewRequest("POST", c.base+"/v1/files?name="+url.QueryEscape(name), bytes.NewReader(data))
	if err != nil {
		return f, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	raw, err := c.raw(req)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(raw, &f)
}

func (c *Client) GetFile(id string) (FileRef, []byte, error) {
	req, err := http.NewRequest("GET", c.base+"/v1/files/"+id, nil)
	if err != nil {
		return FileRef{}, nil, err
	}
	data, err := c.raw(req)
	if err != nil {
		return FileRef{}, nil, err
	}
	mime := http.DetectContentType(data)
	return FileRef{ID: id, Mime: mime, Size: len(data)}, data, nil
}

func (c *Client) raw(req *http.Request) ([]byte, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, req.URL.Path)
	}
	if resp.StatusCode >= 300 {
		var e errResponse
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			if e.Code == "guard" {
				return nil, fmt.Errorf("%w: %s", ErrGuard, strings.TrimPrefix(e.Error, ErrGuard.Error()+": "))
			}
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("%s %s → %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

// ---- CLI ----

// attachAll uploads each path and returns the body with a marker line per file.
func attachAll(b Backend, body string, paths []string) (string, error) {
	var lines []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		f, err := b.PutFile(filepath.Base(p), data)
		if err != nil {
			return "", fmt.Errorf("attach %s: %w", p, err)
		}
		lines = append(lines, Marker(f))
	}
	body = strings.TrimRight(body, "\n")
	if body != "" {
		lines = append([]string{body}, lines...)
	}
	return strings.Join(lines, "\n"), nil
}

// fileGet saves a file to out (default /tmp/gary-files/<id><ext>) and returns the path.
func fileGet(b Backend, id, out string) (string, error) {
	f, data, err := b.GetFile(id)
	if err != nil {
		return "", err
	}
	if out == "" {
		dir := filepath.Join(os.TempDir(), "gary-files")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		out = filepath.Join(dir, id+imageTypes[f.Mime])
	}
	return out, os.WriteFile(out, data, 0o600)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
