package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFilesOnlyTakePictures(t *testing.T) {
	s := testStore(t)
	if _, err := s.PutFile("x.svg", []byte(`<svg onload="alert(1)"></svg>`)); !errors.Is(err, ErrGuard) {
		t.Fatalf("svg/html must be refused: %v", err)
	}
	if _, err := s.PutFile("x.png", nil); !errors.Is(err, ErrGuard) {
		t.Fatalf("empty file: %v", err)
	}
	f, err := s.PutFile("../../etc/shot.png", testPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "shot.png" || f.Mime != "image/png" || len(f.ID) != 32 {
		t.Fatalf("ref: %+v", f)
	}
	g, data, err := s.GetFile(f.ID)
	if err != nil || !bytes.Equal(data, testPNG(t)) || g.Name != "shot.png" {
		t.Fatalf("get: %+v %v", g, err)
	}
	if _, _, err := s.GetFile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAttachOverHTTPAndFetchBack(t *testing.T) {
	c, _ := testHub(t)
	for _, n := range []string{"a", "b"} {
		if err := c.RegisterOn(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(t.TempDir(), "board.png")
	os.WriteFile(p, testPNG(t), 0o600)
	body, err := attachAll(c, "look at this", []string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send("a", "b", body, false); err != nil {
		t.Fatal(err)
	}
	m, err := c.Recv("b")
	if err != nil || m == nil {
		t.Fatal(err)
	}
	sub := markerRe.FindStringSubmatch(m.Body)
	if !strings.HasPrefix(m.Body, "look at this\n[image: board.png — gary file get ") || sub == nil {
		t.Fatalf("body: %q", m.Body)
	}
	out, err := fileGet(c, sub[1], filepath.Join(t.TempDir(), "got.png"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, testPNG(t)) {
		t.Fatal("fetched bytes differ")
	}
	// a picture alone is a message (the marker is its content)
	if body, _ := attachAll(c, "", []string{p}); !markerRe.MatchString(body) || strings.HasPrefix(body, "\n") {
		t.Fatalf("picture-only body: %q", body)
	}
}

func TestGuestFetchesOnlyPicturesInTheirRooms(t *testing.T) {
	srv, s, browser := guestHub(t)
	theirs, _ := s.PutFile("a.png", testPNG(t))
	secret, _ := s.PutFile("b.png", testPNG(t))
	if _, err := s.Post("admin", "shared", Marker(theirs)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post("admin", "ops", Marker(secret)); err != nil {
		t.Fatal(err)
	}
	get := func(id string) int {
		resp, err := browser.Get(srv.URL + "/v1/files/" + id)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(theirs.ID); code != 200 {
		t.Fatalf("guest's own room picture: %d", code)
	}
	if code := get(secret.ID); code != http.StatusNotFound {
		t.Fatalf("picture from a room the guest can't see: %d", code)
	}
	resp, err := browser.Post(srv.URL+"/v1/files?name=n.png", "image/png", bytes.NewReader(testPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("guest upload: %d", resp.StatusCode)
	}
}
