package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ygelfand/posterlink/internal/pool"
	"github.com/ygelfand/posterlink/internal/provider"
)

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 20, 30))
	img.Set(0, 0, color.RGBA{10, 20, 30, 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 20, 30))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type originServer struct {
	*httptest.Server
	hits atomic.Int64
}

func newOrigin(t *testing.T, bodies map[string][]byte) *originServer {
	t.Helper()
	o := &originServer{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.hits.Add(1)
		switch r.URL.Path {
		case "/redirected.jpg":
			http.Redirect(w, r, "/good.jpg", http.StatusFound)
			return
		case "/missing.jpg":
			http.NotFound(w, r)
			return
		case "/boom.jpg":
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case "/html.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("<html>not an image at all</html>"))
			return
		case "/truncated.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes(t)[:12])
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	t.Cleanup(o.Close)
	return o
}

func imgs(urls ...string) []provider.Image {
	out := make([]provider.Image, 0, len(urls))
	for _, u := range urls {
		out = append(out, provider.Image{
			URL:     u,
			Title:   "Blade Runner 2049",
			Creator: "Denis Villeneuve",
			Date:    "2017-10-04",
		})
	}
	return out
}

func src(name string, urls ...string) pool.Source {
	return pool.Source{Name: name, Weight: 1, Images: imgs(urls...)}
}

func newTestServer(t *testing.T, sources []pool.Source) *Server {
	t.Helper()
	s := New(nil, 0, slog.New(slog.DiscardHandler))
	s.pool.Set(sources)
	return s
}

func get(t *testing.T, s *Server, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func TestPosterImageServesBytes(t *testing.T) {
	want := jpegBytes(t)
	origin := newOrigin(t, map[string][]byte{"/good.jpg": want})
	s := newTestServer(t, []pool.Source{src("tmdb", origin.URL+"/good.jpg")})

	resp := get(t, s, "/poster.jpg")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := resp.Header.Get("X-Poster-Provider"); got != "tmdb" {
		t.Errorf("X-Poster-Provider = %q", got)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("missing no-store")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("served bytes are not a decodable JPEG: %v", err)
	}
	if len(body) <= len(want) {
		t.Error("served image is not larger than the origin's, so no metadata was added")
	}
	for _, tag := range []string{"Blade Runner 2049", "Denis Villeneuve", "2017:10:04", origin.URL} {
		if !bytes.Contains(body, []byte(tag)) {
			t.Errorf("served image is missing metadata %q", tag)
		}
	}
	if n, _ := strconv.Atoi(resp.Header.Get("Content-Length")); n != len(body) {
		t.Errorf("Content-Length = %s, want %d", resp.Header.Get("Content-Length"), len(body))
	}
}

func TestPosterImageFollowsRedirects(t *testing.T) {
	origin := newOrigin(t, map[string][]byte{"/good.jpg": jpegBytes(t)})
	s := newTestServer(t, []pool.Source{src("wikidata", origin.URL+"/redirected.jpg")})

	resp := get(t, s, "/poster.jpg")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPosterImageServesPNGAsPNG(t *testing.T) {
	origin := newOrigin(t, map[string][]byte{"/art.png": pngBytes(t)})
	s := newTestServer(t, []pool.Source{src("artic", origin.URL+"/art.png")})

	resp := get(t, s, "/poster.jpg")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("served bytes are not a decodable PNG: %v", err)
	}
}

func TestPosterImageRetriesPastBadImages(t *testing.T) {
	good := jpegBytes(t)
	origin := newOrigin(t, map[string][]byte{"/good.jpg": good})
	s := newTestServer(t, []pool.Source{src("tmdb",
		origin.URL+"/html.jpg",
		origin.URL+"/good.jpg",
	)})

	for range 10 {
		resp := get(t, s, "/poster.jpg")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Poster-Source"); !strings.HasSuffix(got, "/good.jpg") {
			t.Fatalf("served %q, want the valid image", got)
		}
		if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
			t.Fatalf("served bytes are not a decodable JPEG: %v", err)
		}
	}
}

func TestPosterImageRejects(t *testing.T) {
	origin := newOrigin(t, nil)
	for _, path := range []string{"/html.jpg", "/truncated.jpg", "/missing.jpg", "/boom.jpg"} {
		t.Run(path, func(t *testing.T) {
			s := newTestServer(t, []pool.Source{src("tmdb", origin.URL+path)})
			resp := get(t, s, "/poster.jpg")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
		})
	}
}

func TestPosterImageRemembersPermanentFailures(t *testing.T) {
	origin := newOrigin(t, nil)
	s := newTestServer(t, []pool.Source{src("tmdb", origin.URL+"/missing.jpg")})

	resp := get(t, s, "/poster.jpg")
	resp.Body.Close()
	after := origin.hits.Load()
	if s.bad.len() != 1 {
		t.Fatalf("bad set holds %d urls, want 1", s.bad.len())
	}

	resp = get(t, s, "/poster.jpg")
	resp.Body.Close()
	if origin.hits.Load() != after {
		t.Error("a known-bad url was fetched again")
	}

	healthz := get(t, s, "/healthz")
	defer healthz.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(healthz.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["invalid"] != float64(1) {
		t.Errorf("healthz invalid = %v, want 1", body["invalid"])
	}

	s.bad.reset()
	resp = get(t, s, "/poster.jpg")
	resp.Body.Close()
	if origin.hits.Load() == after {
		t.Error("a refreshed pool did not clear the bad set")
	}
}

func TestPosterImageTransientFailureIsNotRemembered(t *testing.T) {
	origin := newOrigin(t, nil)
	s := newTestServer(t, []pool.Source{src("tmdb", origin.URL+"/boom.jpg")})

	resp := get(t, s, "/poster.jpg")
	resp.Body.Close()
	if s.bad.len() != 0 {
		t.Errorf("a 500 was remembered as permanent")
	}
}

func TestPosterImageProviderFilter(t *testing.T) {
	origin := newOrigin(t, map[string][]byte{"/good.jpg": jpegBytes(t), "/other.jpg": jpegBytes(t)})
	s := newTestServer(t, []pool.Source{
		src("tmdb", origin.URL+"/good.jpg"),
		src("steam", origin.URL+"/other.jpg"),
	})

	for range 8 {
		resp := get(t, s, "/poster.jpg?providers=steam")
		resp.Body.Close()
		if got := resp.Header.Get("X-Poster-Provider"); got != "steam" {
			t.Fatalf("X-Poster-Provider = %q, want steam", got)
		}
	}

	resp := get(t, s, "/poster.jpg?providers=nope")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPosterImageWarmingUp(t *testing.T) {
	s := newTestServer(t, nil)
	resp := get(t, s, "/poster.jpg")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestPosterStillRedirects(t *testing.T) {
	origin := newOrigin(t, map[string][]byte{"/good.jpg": jpegBytes(t)})
	s := newTestServer(t, []pool.Source{src("tmdb", origin.URL+"/good.jpg")})

	resp := get(t, s, "/poster")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != origin.URL+"/good.jpg" {
		t.Errorf("Location = %q", loc)
	}
	if origin.hits.Load() != 0 {
		t.Error("the redirect endpoint fetched the image")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		mime string
		ok   bool
	}{
		{"jpeg", jpegBytes(t), "image/jpeg", true},
		{"png", pngBytes(t), "image/png", true},
		{"html", []byte("<html><body>error</body></html>"), "", false},
		{"empty", nil, "", false},
		{"truncated jpeg", jpegBytes(t)[:12], "", false},
		{"webp magic passes through", []byte("RIFF\x00\x00\x00\x00WEBPVP8 padding"), "image/webp", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, err := validate(tc.body)
			if tc.ok != (err == nil) {
				t.Fatalf("validate error = %v, want ok = %v", err, tc.ok)
			}
			if mime != tc.mime {
				t.Errorf("mime = %q, want %q", mime, tc.mime)
			}
		})
	}
}
