package nitrado

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadTargetNeverPrintsItsSecrets(t *testing.T) {
	tg := UploadTarget{url: "https://files.example/upload/SECRETPATH", token: "SECRETTOKEN"}
	j, _ := json.Marshal(struct{ T UploadTarget }{tg})
	for _, s := range []string{fmt.Sprint(tg), fmt.Sprintf("%v %+v %#v %s", tg, tg, tg, tg), string(j)} {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("leak: %s", s)
		}
	}
}

func TestRequestUploadTokenFormAndNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/services/7/gameservers/file_server/upload" ||
			r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("path") != "/games/a/noftp/m/champion" || r.PostForm.Get("file") != "x.json" {
			t.Errorf("form: %v", r.PostForm)
		}
		w.WriteHeader(http.StatusTooManyRequests) // must NOT be retried
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	_, err := c.RequestUploadToken(context.Background(), "7", "/games/a/noftp/m/champion", "x.json")
	var we *WriteError
	if !errors.As(err, &we) || we.Phase != PhaseToken || we.StatusCode != 429 || calls.Load() != 1 {
		t.Fatalf("%v calls=%d", err, calls.Load())
	}
	if _, err := c.RequestUploadToken(context.Background(), "7", "/d", "../x.json"); err == nil {
		t.Fatal("a name with a slash is refused")
	}
}

func TestUploadTokenResponseValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no token":   `{"data":{"token":{"url":"https://f.example/u"}}}`,
		"no url":     `{"data":{"token":{"token":"t"}}}`,
		"not json":   `oops`,
		"http url":   `{"data":{"token":{"url":"http://f.example/u","token":"t"}}}`,
		"userinfo":   `{"data":{"token":{"url":"https://u:p@f.example/u","token":"t"}}}`,
		"no host":    `{"data":{"token":{"url":"https:///u","token":"t"}}}`,
		"javascript": `{"data":{"token":{"url":"javascript:alert(1)","token":"t"}}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		_, err := NewClient(srv.URL, "tok", nil).RequestUploadToken(context.Background(), "7", "/d", "x.json")
		srv.Close()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "f.example") || strings.Contains(err.Error(), `"t"`) {
			t.Errorf("%s: error leaks the destination: %v", name, err)
		}
	}
}

func TestPostUploadSendsExactBytesOnceAndFollowsNoRedirect(t *testing.T) {
	var got []byte
	var calls, redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	mode := "ok"
	fs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("token") != "TKN" || r.Header.Get("Content-Type") != "application/binary" || r.Header.Get("Authorization") != "" {
			t.Errorf("headers: %v", r.Header)
		}
		got, _ = io.ReadAll(r.Body)
		switch mode {
		case "redirect":
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		case "500":
			w.WriteHeader(500)
		}
	}))
	defer fs.Close()
	AllowInsecureUploadURL = true // the local TLS server is 127.0.0.1, outside the real trust boundary
	defer func() { AllowInsecureUploadURL = false }()
	c := NewClient("https://api.invalid", "API-BEARER", fs.Client())
	payload := []byte("{\n  \"Objects\": []\n}\n")
	tg := UploadTarget{url: fs.URL + "/upload/abc", token: "TKN"}
	if err := c.PostUpload(context.Background(), tg, payload); err != nil || string(got) != string(payload) || calls.Load() != 1 {
		t.Fatalf("%v %q %d", err, got, calls.Load())
	}
	mode = "redirect"
	err := c.PostUpload(context.Background(), tg, payload)
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect followed: %v %d", err, redirected.Load())
	}
	mode = "500"
	calls.Store(0)
	err = c.PostUpload(context.Background(), tg, payload)
	var we *WriteError
	if !errors.As(err, &we) || we.Phase != PhaseTransfer || we.Kind != KindTemporary || calls.Load() != 1 {
		t.Fatalf("no retry on 500: %v %d", err, calls.Load())
	}
	if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "TKN") || strings.Contains(err.Error(), fs.URL) {
		t.Fatalf("error leaks: %v", err)
	}
}

func TestPostUploadNetworkErrorIsSanitized(t *testing.T) {
	c := NewClient("https://api.invalid", "x", nil)
	err := c.PostUpload(context.Background(), UploadTarget{url: "https://127.0.0.1:1/upload/SECRETPATH", token: "SECRETTOKEN"}, []byte("x"))
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("%v", err)
	}
}

func TestMkdirIsOneLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/services/7/gameservers/file_server/mkdir" || r.PostForm.Get("path") != "/games/a/m" || r.PostForm.Get("name") != "champion" {
			t.Errorf("%s %v", r.URL.Path, r.PostForm)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	if err := c.Mkdir(context.Background(), "7", "/games/a/m", "champion"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a/b", "..", ".", "", "a\\b"} {
		if c.Mkdir(context.Background(), "7", "/games/a/m", bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestUploadTrustBoundary(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://files.nitrado.net/upload/x":         true,
		"https://nitrado.net/upload/x":               true,
		"https://FS1.Nitrado.NET./upload/x":          true,
		"https://files.nitrado.net:443/upload/x":     true,
		"http://files.nitrado.net/upload/x":          false, // not https
		"https://files.nitrado.net:8443/upload/x":    false, // non-default port
		"https://evil.example/upload/x":              false, // foreign host
		"https://nitrado.net.evil.example/upload/x":  false, // look-alike suffix
		"https://evilnitrado.net/upload/x":           false, // not a subdomain
		"https://203.0.113.7/upload/x":               false, // IP literal
		"https://[2001:db8::1]/upload/x":             false,
		"https://user:pw@files.nitrado.net/upload/x": false, // user-info
		"https:files.nitrado.net":                    false, // opaque
		"//files.nitrado.net/upload":                 false, // no scheme
	} {
		if got := checkUploadURL(raw) == nil; got != want {
			t.Errorf("%s: trusted=%t want %t", raw, got, want)
		}
	}
	// PostUpload re-checks the boundary itself: a target that is outside it is never contacted.
	var hit atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer srv.Close()
	err := NewClient("https://api.invalid", "x", srv.Client()).PostUpload(context.Background(), UploadTarget{url: srv.URL + "/u", token: "T"}, []byte("x"))
	if !errors.Is(err, ErrInsecureUploadURL) || hit.Load() != 0 {
		t.Fatalf("%v hits=%d", err, hit.Load())
	}
}
