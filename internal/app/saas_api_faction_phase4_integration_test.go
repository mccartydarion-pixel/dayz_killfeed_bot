//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionassets"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// End-to-end Faction Hub Phase 4 tests over the real routes and a real PostgreSQL: logo upload,
// replacement, deletion and public serving; leadership transfer; self-leave; catalog validation.

func testPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testJPEG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{200, uint8(x), uint8(y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testWebP is a structurally valid lossless WebP header of the given size (the validator checks
// the container and header, it does not decode pixels).
func testWebP(w, h int) []byte {
	payload := []byte{0x2F}
	payload = binary.LittleEndian.AppendUint32(payload, uint32(w-1)|uint32(h-1)<<14)
	payload = append(payload, make([]byte, 8)...)
	chunk := append([]byte("VP8L"), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	chunk = append(chunk, payload...)
	body := append([]byte("WEBP"), chunk...)
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...)
}

// multipartBody builds a body with the given parts: name, filename, content type, data.
type mpPart struct {
	field, filename, contentType string
	data                         []byte
}

func multipartBody(t testing.TB, parts ...mpPart) (contentType string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disp := fmt.Sprintf(`form-data; name="%s"`, p.field)
		if p.filename != "" {
			disp += fmt.Sprintf(`; filename="%s"`, strings.ReplaceAll(p.filename, `"`, `%22`))
		}
		h.Set("Content-Disposition", disp)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = pw.Write(p.data)
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

// rawUpload sends body with an explicit Content-Type header.
func (w *factionWorld) rawUpload(f installationFixture, factionID int64, actor, contentType string, body []byte) *apiResult {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.base+w.path(f, fmt.Sprintf("/%d/logo", factionID)), bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")
	if actor != "" {
		req.Header.Set(actingUserHeader, actor)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return &apiResult{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: raw}
}

func (w *factionWorld) upload(f installationFixture, factionID int64, actor, filename, declared string, data []byte) *apiResult {
	ct, body := multipartBody(w.t, mpPart{"file", filename, declared, data})
	return w.rawUpload(f, factionID, actor, ct, body)
}

type publicResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// publicGet fetches a path from the public asset route with NO credentials at all.
func (w *factionWorld) publicGet(method, path string, hdr map[string]string) publicResponse {
	w.t.Helper()
	req, err := http.NewRequest(method, w.base+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return publicResponse{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

// logoPath turns the API's absolute logo URL into the path on the test listener.
func logoPath(t testing.TB, logo map[string]any) string {
	t.Helper()
	url, _ := logo["url"].(string)
	const base = "https://champion.example"
	if !strings.HasPrefix(url, base+"/assets/faction-logos/") {
		t.Fatalf("logo url must be absolute on the configured public base: %q", url)
	}
	return strings.TrimPrefix(url, base)
}

func (w *factionWorld) leaderWithFaction(name, tag string) (leader string, factionID int64, fp string) {
	w.t.Helper()
	leader = w.players[0]
	f := w.createFaction(w.a1, leader, name, tag, "OPEN")
	return leader, idOf(f), fmt.Sprintf("/%d", idOf(f))
}

// joined makes user a plain member of the faction via the real apply/accept flow.
func (w *factionWorld) joined(leader string, factionID int64, user string) int64 {
	w.t.Helper()
	fp := fmt.Sprintf("/%d", factionID)
	app := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), user, nil), http.StatusCreated, "apply").JSON(w.t)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/accept", fp, idOf(app))), leader, nil), http.StatusOK, "accept")
	mem := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/members"), leader, nil), http.StatusOK, "members").JSON(w.t)
	for _, it := range mem["items"].([]any) {
		m := it.(map[string]any)
		if m["discordUserId"] == user {
			return idOf(m)
		}
	}
	w.t.Fatal("joined member not found")
	return 0
}

func (w *factionWorld) memberIDByDiscord(factionID int64, viewer, discordID string) int64 {
	w.t.Helper()
	mem := w.expect(w.do(http.MethodGet, w.path(w.a1, fmt.Sprintf("/%d/members", factionID)), viewer, nil), http.StatusOK, "members").JSON(w.t)
	for _, it := range mem["items"].([]any) {
		if m := it.(map[string]any); m["discordUserId"] == discordID {
			return idOf(m)
		}
	}
	return 0
}

// --- logo upload / serve / replace / delete ---------------------------------------------------------------

func TestFactionLogoLifecycleAndPublicServing(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Logo Legion", "LL")
	other := w.players[5]

	// A faction starts on the Champion default: logo is null everywhere.
	prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), other, nil), http.StatusOK, "profile").JSON(t)
	if v, ok := prof["logo"]; !ok || v != nil {
		t.Fatalf("no logo yet: logo must be present and null: %v", prof["logo"])
	}

	// Upload (PNG) -> 201 with safe metadata only.
	img := testPNG(t, 300, 200)
	res := w.expect(w.upload(w.a1, fid, leader, "Legion Crest.PNG", "image/png", img), http.StatusCreated, "first upload").JSON(t)
	logo := res["logo"].(map[string]any)
	if res["replaced"] != false || logo["contentType"] != "image/png" || logo["width"].(float64) != 300 || logo["height"].(float64) != 200 || logo["id"] == nil {
		t.Fatalf("upload response: %v", res)
	}
	if len(logo) != 5 {
		t.Fatalf("the logo object must carry exactly id,url,contentType,width,height, got %v", logo)
	}
	for k := range logo {
		if strings.Contains(strings.ToLower(k), "key") || strings.Contains(strings.ToLower(k), "bucket") || strings.Contains(strings.ToLower(k), "credential") {
			t.Fatalf("no storage detail may leak through the response: %q", k)
		}
	}
	url := logo["url"].(string)
	if !strings.HasSuffix(url, ".png") || strings.Contains(url, "Legion") {
		t.Fatalf("the URL uses the server-generated id, never the uploaded name: %s", url)
	}
	if w.store.Len() != 1 {
		t.Fatalf("one stored object expected, got %d", w.store.Len())
	}

	// The public logo endpoint needs no credentials and serves the exact bytes, safely.
	pub := w.publicGet(http.MethodGet, logoPath(t, logo), nil)
	if pub.Status != http.StatusOK || !bytes.Equal(pub.Body, img) {
		t.Fatalf("public GET: %d, %d bytes (want %d)", pub.Status, len(pub.Body), len(img))
	}
	for k, want := range map[string]string{
		"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Cross-Origin-Resource-Policy": "cross-origin",
		"Cache-Control": "public, max-age=31536000, immutable", "Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if got := pub.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	etag := pub.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if nm := w.publicGet(http.MethodGet, logoPath(t, logo), map[string]string{"If-None-Match": etag}); nm.Status != http.StatusNotModified || len(nm.Body) != 0 {
		t.Fatalf("conditional GET: %d", nm.Status)
	}
	if head := w.publicGet(http.MethodHead, logoPath(t, logo), nil); head.Status != http.StatusOK || len(head.Body) != 0 {
		t.Fatalf("HEAD: %d", head.Status)
	}
	if bad := w.publicGet(http.MethodGet, strings.TrimSuffix(logoPath(t, logo), ".png")+".jpg", nil); bad.Status != http.StatusNotFound {
		t.Fatalf("the URL names the stored format: %d", bad.Status)
	}
	if post := w.publicGet(http.MethodPost, logoPath(t, logo), nil); post.Status != http.StatusMethodNotAllowed {
		t.Fatalf("POST on the public route: %d", post.Status)
	}

	// Every faction representation carries the same logo object, so the public Hub needs no
	// privileged call to render it.
	prof = w.expect(w.do(http.MethodGet, w.path(w.a1, fp), other, nil), http.StatusOK, "profile with logo").JSON(t)
	if pl := prof["logo"].(map[string]any); pl["url"] != url || pl["id"] != logo["id"] {
		t.Fatalf("profile logo: %v", prof["logo"])
	}
	dir := w.expect(w.do(http.MethodGet, w.path(w.a1, ""), other, nil), http.StatusOK, "directory").JSON(t)["items"].([]any)
	if dl := dir[0].(map[string]any)["logo"].(map[string]any); dl["url"] != url {
		t.Fatalf("directory logo: %v", dir[0])
	}
	me := w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), leader, nil), http.StatusOK, "me").JSON(t)
	if ml := me["faction"].(map[string]any)["logo"].(map[string]any); ml["url"] != url {
		t.Fatalf("me logo: %v", me)
	}

	// Replacement (a JPEG this time): 200 replaced=true, the old URL stops resolving and its
	// bytes are gone; exactly one object remains.
	jpg := testJPEG(t, 256, 256)
	res = w.expect(w.upload(w.a1, fid, leader, "new.jpg", "image/jpeg", jpg), http.StatusOK, "replacement").JSON(t)
	newLogo := res["logo"].(map[string]any)
	if res["replaced"] != true || newLogo["contentType"] != "image/jpeg" || newLogo["url"] == url || !strings.HasSuffix(newLogo["url"].(string), ".jpg") {
		t.Fatalf("replacement response: %v", res)
	}
	if old := w.publicGet(http.MethodGet, logoPath(t, logo), nil); old.Status != http.StatusNotFound {
		t.Fatalf("the replaced logo's URL must 404, got %d", old.Status)
	}
	if now := w.publicGet(http.MethodGet, logoPath(t, newLogo), nil); now.Status != http.StatusOK || !bytes.Equal(now.Body, jpg) || now.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("new logo: %d", now.Status)
	}
	if w.store.Len() != 1 {
		t.Fatalf("replacement must leave exactly one stored object, got %d", w.store.Len())
	}
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, fid, 1)

	// WebP is accepted too.
	w.expect(w.upload(w.a1, fid, leader, "x.webp", "image/webp", testWebP(512, 512)), http.StatusOK, "webp replacement")

	// Delete: back to the default logo; idempotent; bytes gone; URL 404.
	cur := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), leader, nil), http.StatusOK, "profile").JSON(t)["logo"].(map[string]any)
	del := w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/logo"), leader, nil), http.StatusOK, "delete").JSON(t)
	if del["deleted"] != true || del["logo"] != nil {
		t.Fatalf("delete response: %v", del)
	}
	if w.store.Len() != 0 {
		t.Fatalf("delete must remove the bytes, %d left", w.store.Len())
	}
	if gone := w.publicGet(http.MethodGet, logoPath(t, cur), nil); gone.Status != http.StatusNotFound {
		t.Fatalf("deleted logo URL: %d", gone.Status)
	}
	again := w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/logo"), leader, nil), http.StatusOK, "delete again").JSON(t)
	if again["deleted"] != false || again["logo"] != nil {
		t.Fatalf("second delete is a no-op: %v", again)
	}
	prof = w.expect(w.do(http.MethodGet, w.path(w.a1, fp), other, nil), http.StatusOK, "profile after delete").JSON(t)
	if prof["logo"] != nil {
		t.Fatalf("back on the default logo: %v", prof["logo"])
	}
}

func assertRows(t testing.TB, w *factionWorld, sql string, arg int64, want int) {
	t.Helper()
	var n int
	if err := w.a.DB.Pool.QueryRow(context.Background(), sql, arg).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%s: got %d, want %d", sql, n, want)
	}
}

func TestFactionLogoAuthorizationAndTenancy(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Guarded Logo", "GL")
	officer, member, outsider := w.players[1], w.players[2], w.players[3]
	w.joined(leader, fid, member)
	oid := w.joined(leader, fid, officer)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/members/%d/promote", fp, oid)), leader, nil), http.StatusOK, "promote")
	img := testPNG(t, 256, 256)

	for who, actor := range map[string]string{"officer": officer, "member": member, "outsider": outsider, "org OWNER": w.a1.OwnerDiscordID, "org ADMIN": w.admin, "org MEMBER": w.member} {
		w.expect(w.upload(w.a1, fid, actor, "a.png", "image/png", img), http.StatusForbidden, who+" uploading")
		w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/logo"), actor, nil), http.StatusForbidden, who+" deleting")
	}
	if w.store.Len() != 0 {
		t.Fatalf("a forbidden upload must store nothing, got %d objects", w.store.Len())
	}
	// Authorization comes BEFORE the body is read: an outsider posting an oversized body is 403, never 413.
	huge := append(append([]byte(nil), img...), make([]byte, 6<<20)...)
	w.expect(w.upload(w.a1, fid, outsider, "a.png", "image/png", huge), http.StatusForbidden, "outsider with an oversized body")
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, fid, 0)
	// Unauthenticated / unsynced callers.
	ct, body := multipartBody(t, mpPart{"file", "a.png", "image/png", img})
	req, _ := http.NewRequest(http.MethodPost, w.base+w.path(w.a1, fp+"/logo"), bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no service auth: %d", resp.StatusCode)
	}
	w.expect(w.rawUpload(w.a1, fid, "", ct, body), http.StatusUnauthorized, "no acting user")

	// Tenant isolation: another organization's / installation's path never reaches this faction.
	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID}
	for _, scope := range []installationFixture{crossOrg, crossInst} {
		w.expect(w.upload(scope, fid, leader, "a.png", "image/png", img), http.StatusNotFound, "cross-tenant upload")
		w.expect(w.do(http.MethodDelete, w.path(scope, fp+"/logo"), leader, nil), http.StatusNotFound, "cross-tenant delete")
		w.expect(w.do(http.MethodPost, w.path(scope, fp+"/leave"), member, nil), http.StatusNotFound, "cross-tenant leave")
		w.expect(w.do(http.MethodPost, w.path(scope, fp+"/transfer-leadership"), leader, map[string]any{"memberId": oid}), http.StatusNotFound, "cross-tenant transfer")
	}
	// The leader of tenant B cannot touch tenant A's logo through B's own path either.
	bLeader := w.players[4]
	fb := w.createFaction(w.b1, bLeader, "Tenant B", "TB", "OPEN")
	w.expect(w.upload(w.b1, fid, bLeader, "a.png", "image/png", img), http.StatusNotFound, "A's faction id under B")
	if got := w.expect(w.upload(w.b1, idOf(fb), bLeader, "a.png", "image/png", img), http.StatusCreated, "B uploads its own").JSON(t); got["logo"] == nil {
		t.Fatal("tenant B's own upload works")
	}
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, fid, 0)
}

func TestFactionLogoUploadRejectsUnsafeAndInvalidFiles(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Strict Gate", "SG")
	good := testPNG(t, 256, 256)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"><script>alert(1)</script></svg>`)
	html := []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>")
	huge := append(append([]byte(nil), good...), make([]byte, 6<<20)...)

	type tc struct {
		name     string
		filename string
		declared string
		data     []byte
		want     int
	}
	cases := []tc{
		{"svg", "logo.svg", "image/svg+xml", svg, http.StatusUnsupportedMediaType},
		{"svg renamed png with png type", "logo.png", "image/png", svg, http.StatusUnsupportedMediaType},
		{"html renamed png", "logo.png", "image/png", html, http.StatusUnsupportedMediaType},
		{"text renamed jpg", "logo.jpg", "image/jpeg", []byte(strings.Repeat("hello logo ", 500)), http.StatusUnsupportedMediaType},
		{"gif", "logo.gif", "image/gif", append([]byte("GIF89a"), make([]byte, 400)...), http.StatusUnsupportedMediaType},
		{"pdf", "logo.pdf", "application/pdf", append([]byte("%PDF-1.7\n"), make([]byte, 400)...), http.StatusUnsupportedMediaType},
		{"real png but declared svg", "logo.png", "image/svg+xml", good, http.StatusUnsupportedMediaType},
		{"real png but declared jpeg", "logo.jpg", "image/jpeg", good, http.StatusUnsupportedMediaType},
		{"empty file", "logo.png", "image/png", nil, http.StatusBadRequest},
		{"corrupt png", "logo.png", "image/png", good[:len(good)/2], http.StatusBadRequest},
		{"corrupt jpeg", "logo.jpg", "image/jpeg", testJPEG(t, 256, 256)[:200], http.StatusBadRequest},
		{"too small", "logo.png", "image/png", testPNG(t, 64, 64), http.StatusBadRequest},
		{"too large in pixels", "logo.png", "image/png", testPNG(t, 2100, 300), http.StatusBadRequest},
		{"oversized body", "logo.png", "image/png", huge, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		r := w.upload(w.a1, fid, leader, c.filename, c.declared, c.data)
		if r.Status != c.want {
			t.Errorf("%s: want %d, got %d %s", c.name, c.want, r.Status, r.Body)
		}
	}
	if w.store.Len() != 0 {
		t.Fatalf("no rejected upload may store anything, got %d objects", w.store.Len())
	}
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, fid, 0)

	// Body shape: only multipart with exactly one "file" part.
	b64 := base64.StdEncoding.EncodeToString(good)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/logo"), leader, map[string]any{"file": b64}), http.StatusUnsupportedMediaType, "base64 JSON blob")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/logo"), leader, map[string]any{"externalUrl": "https://evil.example/logo.png"}), http.StatusUnsupportedMediaType, "external URL as JSON")
	w.expect(w.rawUpload(w.a1, fid, leader, "image/png", good), http.StatusUnsupportedMediaType, "raw image body")
	w.expect(w.rawUpload(w.a1, fid, leader, "multipart/form-data", good), http.StatusUnsupportedMediaType, "multipart without a boundary")
	for name, parts := range map[string][]mpPart{
		"external url field": {{"externalUrl", "", "", []byte("https://evil.example/logo.png")}},
		"url beside file":    {{"file", "a.png", "image/png", good}, {"url", "", "", []byte("https://evil.example/logo.png")}},
		"svg markup field":   {{"svg", "", "", svg}},
		"base64 text field":  {{"logo", "", "", []byte(b64)}},
		"wrong field name":   {{"image", "a.png", "image/png", good}},
		"two files":          {{"file", "a.png", "image/png", good}, {"file", "b.png", "image/png", good}},
		"no parts":           {},
	} {
		ct, body := multipartBody(t, parts...)
		w.expect(w.rawUpload(w.a1, fid, leader, ct, body), http.StatusBadRequest, name)
	}
	if w.store.Len() != 0 {
		t.Fatalf("body-shape rejections must store nothing, got %d", w.store.Len())
	}

	// A hostile file name is never a path: the upload succeeds under a server-generated key and the
	// recorded name is reduced to a harmless base name.
	for _, name := range []string{"../../etc/passwd.png", `..\..\windows\system32\cmd.png`, "a/b/c/../../evil.png", `"><script>.png`} {
		r := w.upload(w.a1, fid, leader, name, "image/png", good)
		if r.Status != http.StatusCreated && r.Status != http.StatusOK {
			t.Fatalf("file name %q: %d %s", name, r.Status, r.Body)
		}
		var key, orig string
		if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT storage_key, original_filename FROM hub_faction_assets WHERE faction_id=$1`, fid).Scan(&key, &orig); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(key, "..") || strings.Contains(key, "passwd") || strings.Contains(key, "windows") || strings.Contains(key, "evil") || strings.ContainsAny(key, `\<>" `) {
			t.Fatalf("storage key must not derive from the upload name: %q (for %q)", key, name)
		}
		if !strings.HasPrefix(key, fmt.Sprintf("factions/%d/%d/%d/", w.a1.OrgID, w.a1.InstallationID, fid)) || !strings.HasSuffix(key, ".png") {
			t.Fatalf("unexpected key layout: %q", key)
		}
		if strings.ContainsAny(orig, `/\<>"`) || strings.Contains(orig, "..") || len(orig) > 100 {
			t.Fatalf("recorded file name must be sanitized: %q (for %q)", orig, name)
		}
	}
	if w.store.Len() != 1 {
		t.Fatalf("five replacements leave exactly one object, got %d", w.store.Len())
	}
	// A NUL byte in the file name makes the multipart header malformed: refused outright.
	w.expect(w.upload(w.a1, fid, leader, "logo\x00.png", "image/png", good), http.StatusBadRequest, "NUL in file name")
	// An octet-stream declared type is not a contradiction (some clients send it for everything).
	w.expect(w.upload(w.a1, fid, leader, "a.png", "application/octet-stream", good), http.StatusOK, "octet-stream declared")
	w.expect(w.upload(w.a1, fid, leader, "a.png", "", good), http.StatusOK, "no declared type")
}

func TestFactionLogoStorageFailuresAreSafe(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Fragile Store", "FS")
	img := testPNG(t, 256, 256)
	first := w.expect(w.upload(w.a1, fid, leader, "a.png", "image/png", img), http.StatusCreated, "first").JSON(t)["logo"].(map[string]any)

	// 1. The byte store fails on Put: nothing is recorded, the current logo is untouched.
	w.store.PutErr = errors.New("store down")
	if r := w.upload(w.a1, fid, leader, "b.png", "image/png", testPNG(t, 300, 300)); r.Status != http.StatusInternalServerError || strings.Contains(string(r.Body), "store down") {
		t.Fatalf("store failure must be a plain 500 without detail: %d %s", r.Status, r.Body)
	}
	w.store.PutErr = nil
	cur := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), leader, nil), http.StatusOK, "profile").JSON(t)["logo"].(map[string]any)
	if cur["id"] != first["id"] {
		t.Fatal("a failed replacement must keep the current logo")
	}
	if pub := w.publicGet(http.MethodGet, logoPath(t, first), nil); pub.Status != http.StatusOK {
		t.Fatalf("the current logo must still be served: %d", pub.Status)
	}

	// 2. The metadata transaction fails (the actor is not the leader by the time it runs): the
	// bytes just written are rolled back, the old logo stays.
	svc := factionassets.NewService(w.store, w.a.FactionHub)
	before := w.store.Len()
	_, err := svc.UploadLogo(context.Background(), factionassets.UploadInput{OrganizationID: w.a1.OrgID, InstallationID: w.a1.InstallationID, FactionID: fid,
		ActorUserID: mustAppUserID(t, w.a, w.players[3]), Data: img, DeclaredType: "image/png", Filename: "x.png"})
	if !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("non-leader through the service: %v", err)
	}
	if w.store.Len() != before {
		t.Fatalf("a failed metadata transaction must delete the bytes it wrote: %d -> %d", before, w.store.Len())
	}

	// 3. Deleting the OLD bytes fails after a successful replacement: the new logo is live, the
	// stale object is an orphan, and the sweep removes it (but never a referenced or a fresh one).
	w.store.DeleteErr = errors.New("delete failed")
	second := w.expect(w.upload(w.a1, fid, leader, "c.png", "image/png", testPNG(t, 320, 320)), http.StatusOK, "replacement with failing delete").JSON(t)["logo"].(map[string]any)
	w.store.DeleteErr = nil
	if w.store.Len() != 2 {
		t.Fatalf("the orphaned old object should remain until swept, got %d objects", w.store.Len())
	}
	if pub := w.publicGet(http.MethodGet, logoPath(t, second), nil); pub.Status != http.StatusOK {
		t.Fatal("the new logo must be live")
	}
	if pub := w.publicGet(http.MethodGet, logoPath(t, first), nil); pub.Status != http.StatusNotFound {
		t.Fatal("the replaced logo's URL is gone even though its bytes linger")
	}
	// Too fresh to sweep: an in-flight upload (bytes written, metadata not yet committed) is safe.
	if n, err := w.a.FactionAssets.SweepOrphans(context.Background(), time.Hour, 50, 3); err != nil || n != 0 {
		t.Fatalf("a fresh orphan must not be swept yet: %d %v", n, err)
	}
	// Old enough: swept. The referenced object survives.
	w.store.AgeAll(3 * time.Hour)
	if n, err := w.a.FactionAssets.SweepOrphans(context.Background(), time.Hour, 50, 3); err != nil || n != 1 {
		t.Fatalf("exactly the orphan must be swept: %d %v", n, err)
	}
	if w.store.Len() != 1 {
		t.Fatalf("exactly the referenced object must remain, got %d", w.store.Len())
	}
	if pub := w.publicGet(http.MethodGet, logoPath(t, second), nil); pub.Status != http.StatusOK {
		t.Fatal("the referenced logo must survive the sweep")
	}
	// A deleted faction's bytes are orphans too.
	if _, err := w.a.DB.Pool.Exec(context.Background(), `DELETE FROM hub_factions WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	w.store.AgeAll(3 * time.Hour)
	if n, err := w.a.FactionAssets.SweepOrphans(context.Background(), time.Hour, 50, 3); err != nil || n != 1 || w.store.Len() != 0 {
		t.Fatalf("a deleted faction's bytes must be swept: %d %v left=%d", n, err, w.store.Len())
	}
}

func TestFactionLogoRateLimit(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, _ := w.leaderWithFaction("Spam Guard", "SPG")
	w.a.saasFactionLogoLimiter = newSaaSRateLimiter(time.Hour, 2)
	img := testPNG(t, 256, 256)
	w.expect(w.upload(w.a1, fid, leader, "a.png", "image/png", img), http.StatusCreated, "upload 1")
	w.expect(w.upload(w.a1, fid, leader, "a.png", "image/png", img), http.StatusOK, "upload 2")
	r := w.expect(w.upload(w.a1, fid, leader, "a.png", "image/png", img), http.StatusTooManyRequests, "upload 3")
	if r.errCode(t) != "RATE_LIMITED" {
		t.Fatalf("rate limit code: %s", r.Body)
	}
	// An unauthorized caller is refused (403) before the limiter, so they cannot burn the leader's budget.
	w.expect(w.upload(w.a1, fid, w.players[3], "a.png", "image/png", img), http.StatusForbidden, "outsider is 403, not 429")
	if w.store.Len() != 1 {
		t.Fatalf("one object expected, got %d", w.store.Len())
	}
}

// --- flag / armband / color validation ------------------------------------------------------------------------

func TestFactionVisualKeysAreCatalogValidated(t *testing.T) {
	w := newFactionWorld(t)
	leader, _, fp := w.leaderWithFaction("Colors And Flags", "CF")
	put := func(body map[string]any) *apiResult { return w.do(http.MethodPut, w.path(w.a1, fp), leader, body) }

	got := w.expect(put(map[string]any{"flagKey": "red", "armbandKey": " Orange ", "primaryColor": "#d4af37", "secondaryColor": "#2b2f33"}), http.StatusOK, "approved keys").JSON(t)
	if got["flagKey"] != "RED" || got["armbandKey"] != "ORANGE" || got["primaryColor"] != "#D4AF37" || got["secondaryColor"] != "#2B2F33" {
		t.Fatalf("normalized keys: %v", got)
	}
	dir := w.expect(w.do(http.MethodGet, w.path(w.a1, ""), w.players[5], nil), http.StatusOK, "directory").JSON(t)["items"].([]any)[0].(map[string]any)
	if dir["flagKey"] != "RED" || dir["armbandKey"] != "ORANGE" {
		t.Fatalf("directory carries the keys: %v", dir)
	}
	for _, flag := range factionhub.DayzFlags {
		w.expect(put(map[string]any{"flagKey": flag}), http.StatusOK, "flag "+flag)
	}
	for _, band := range factionhub.Armbands {
		w.expect(put(map[string]any{"armbandKey": band}), http.StatusOK, "armband "+band)
	}
	// Invalid catalog values, URLs and injection attempts are all 400 and change nothing.
	for name, body := range map[string]map[string]any{
		"unknown flag":      {"flagKey": "chernarus"},
		"flag url":          {"flagKey": "https://evil.example/flag.png"},
		"armband as flag":   {"flagKey": "ORANGE"},
		"unknown armband":   {"armbandKey": "PURPLE"},
		"armband url":       {"armbandKey": "url(https://evil.example/x)"},
		"armband injection": {"armbandKey": "RED;background:red"},
		"rgb color":         {"primaryColor": "rgb(1,2,3)"},
		"url color":         {"primaryColor": "url(https://evil.example/x)"},
		"var color":         {"secondaryColor": "var(--x)"},
		"expression color":  {"secondaryColor": "expression(alert(1))"},
		"semicolon color":   {"primaryColor": "#aabbcc;background:url(x)"},
		"named color":       {"primaryColor": "red"},
		"short hex color":   {"primaryColor": "#abc"},
		"logo key":          {"logoKey": "wolf"},
		"logo url":          {"logoUrl": "https://evil.example/x.png"},
		"logo asset id":     {"logoAssetId": 1},
	} {
		w.expect(put(body), http.StatusBadRequest, name)
	}
	// Clearing: "" removes a key/color.
	got = w.expect(put(map[string]any{"flagKey": "", "armbandKey": "", "primaryColor": "", "secondaryColor": ""}), http.StatusOK, "clear").JSON(t)
	if got["flagKey"] != nil || got["armbandKey"] != nil || got["primaryColor"] != nil || got["secondaryColor"] != nil {
		t.Fatalf("cleared: %v", got)
	}
	// Only the leader may set them.
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), w.players[3], map[string]any{"flagKey": "RED"}), http.StatusForbidden, "outsider")
}

// --- leadership transfer -------------------------------------------------------------------------------------------

func TestFactionLeadershipTransferHTTP(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Line Of Succession", "LOS")
	officer, member, outsider := w.players[1], w.players[2], w.players[3]
	mMember := w.joined(leader, fid, member)
	mOfficer := w.joined(leader, fid, officer)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/members/%d/promote", fp, mOfficer)), leader, nil), http.StatusOK, "promote")
	tp := w.path(w.a1, fp+"/transfer-leadership")
	leaderMember := w.memberIDByDiscord(fid, leader, leader)

	// Authorization: only the LEADER (org roles gain nothing).
	for who, actor := range map[string]string{"officer": officer, "member": member, "outsider": outsider, "org OWNER": w.a1.OwnerDiscordID, "org ADMIN": w.admin} {
		w.expect(w.do(http.MethodPost, tp, actor, map[string]any{"memberId": mMember}), http.StatusForbidden, who+" transferring")
	}
	// Bad bodies.
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{}), http.StatusBadRequest, "no memberId")
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": 0}), http.StatusBadRequest, "zero memberId")
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": mMember, "userId": 5}), http.StatusBadRequest, "unknown field")
	w.expect(w.do(http.MethodPost, tp, leader, `{"memberId":"x"}`), http.StatusBadRequest, "non-numeric memberId")
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": 999999999}), http.StatusNotFound, "unknown member")
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": leaderMember}), http.StatusConflict, "transfer to self")
	// A member of ANOTHER faction is not a valid target.
	otherLeader := w.players[4]
	of := w.createFaction(w.a1, otherLeader, "Rival Corps", "RC", "OPEN")
	otherMember := w.memberIDByDiscord(idOf(of), otherLeader, otherLeader)
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": otherMember}), http.StatusNotFound, "cross-faction target")

	// LEADER -> MEMBER.
	res := w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": mMember}), http.StatusOK, "transfer").JSON(t)
	nl, pl := res["leader"].(map[string]any), res["previousLeader"].(map[string]any)
	if nl["discordUserId"] != member || nl["role"] != "LEADER" || pl["discordUserId"] != leader || pl["role"] != "OFFICER" {
		t.Fatalf("transfer response: %v", res)
	}
	prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), member, nil), http.StatusOK, "profile").JSON(t)
	if l := prof["leader"].(map[string]any); l["discordUserId"] != member || prof["viewer"].(map[string]any)["role"] != "LEADER" || len(prof["officers"].([]any)) != 2 {
		t.Fatalf("profile after transfer: leader=%v officers=%v", prof["leader"], prof["officers"])
	}
	// The former leader lost the leader powers immediately.
	w.expect(w.do(http.MethodPost, tp, leader, map[string]any{"memberId": mOfficer}), http.StatusForbidden, "former leader transferring")
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"description": "still me"}), http.StatusForbidden, "former leader editing")
	w.expect(w.upload(w.a1, fid, leader, "a.png", "image/png", testPNG(t, 256, 256)), http.StatusForbidden, "former leader uploading a logo")
	w.expect(w.upload(w.a1, fid, member, "a.png", "image/png", testPNG(t, 256, 256)), http.StatusCreated, "new leader uploading a logo")
	// LEADER -> OFFICER.
	res = w.expect(w.do(http.MethodPost, tp, member, map[string]any{"memberId": mOfficer}), http.StatusOK, "transfer to officer").JSON(t)
	if res["leader"].(map[string]any)["discordUserId"] != officer {
		t.Fatalf("second transfer: %v", res)
	}
}

func TestFactionLeadershipTransferRaceHTTP(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Two Claimants", "TC")
	a, b := w.players[1], w.players[2]
	mA, mB := w.joined(leader, fid, a), w.joined(leader, fid, b)
	tp := w.path(w.a1, fp+"/transfer-leadership")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, target := range []int64{mA, mB} {
		wg.Add(1)
		go func(i int, target int64) {
			defer wg.Done()
			<-start
			codes[i] = w.do(http.MethodPost, tp, leader, map[string]any{"memberId": target}).Status
		}(i, target)
	}
	close(start)
	wg.Wait()
	ok, forbidden := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			forbidden++
		}
	}
	if ok != 1 || forbidden != 1 {
		t.Fatalf("exactly one transfer wins (200) and the other is 403, got %v", codes)
	}
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, fid, 1)
}

// --- self leave ---------------------------------------------------------------------------------------------------------

func TestFactionSelfLeaveHTTP(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Open Exits", "OE")
	officer, member, outsider := w.players[1], w.players[2], w.players[3]
	w.joined(leader, fid, member)
	mOfficer := w.joined(leader, fid, officer)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/members/%d/promote", fp, mOfficer)), leader, nil), http.StatusOK, "promote")
	lp := w.path(w.a1, fp+"/leave")

	// The LEADER cannot leave: a stable machine-readable code.
	r := w.expect(w.do(http.MethodPost, lp, leader, nil), http.StatusConflict, "leader leaving")
	if r.errCode(t) != "LEADERSHIP_TRANSFER_REQUIRED" {
		t.Fatalf("error code: %s", r.Body)
	}
	assertRows(t, w, `SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, fid, 1)
	// A non-member is refused.
	w.expect(w.do(http.MethodPost, lp, outsider, nil), http.StatusForbidden, "outsider leaving")
	// An org OWNER/ADMIN is not a member either: no special path.
	w.expect(w.do(http.MethodPost, lp, w.admin, nil), http.StatusForbidden, "org admin leaving")

	// MEMBER leaves; a repeat is a safe 403.
	res := w.expect(w.do(http.MethodPost, lp, member, nil), http.StatusOK, "member leaving").JSON(t)
	if res["left"] != true || res["member"].(map[string]any)["discordUserId"] != member || res["member"].(map[string]any)["role"] != "MEMBER" {
		t.Fatalf("leave response: %v", res)
	}
	w.expect(w.do(http.MethodPost, lp, member, nil), http.StatusForbidden, "second leave")
	me := w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), member, nil), http.StatusOK, "me").JSON(t)
	if me["faction"] != nil || me["role"] != nil {
		t.Fatalf("after leaving the user has no faction: %v", me)
	}
	// The former member can found a faction, or apply anywhere, right away.
	w.createFaction(w.a1, member, "Fresh Banner", "FB", "OPEN")
	// OFFICER leaves and can apply back.
	w.expect(w.do(http.MethodPost, lp, officer, nil), http.StatusOK, "officer leaving")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), officer, nil), http.StatusCreated, "former officer re-applying")
	// After a handover the old leader may leave.
	stay := w.players[4]
	mStay := w.joined(leader, fid, stay)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/transfer-leadership"), leader, map[string]any{"memberId": mStay}), http.StatusOK, "handover")
	w.expect(w.do(http.MethodPost, lp, leader, nil), http.StatusOK, "former leader leaving")
	prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), stay, nil), http.StatusOK, "profile").JSON(t)
	if prof["memberCount"].(float64) != 1 || prof["leader"].(map[string]any)["discordUserId"] != stay {
		t.Fatalf("faction after everyone else left: %v", prof)
	}
	// The last member is the leader: they cannot leave (there is no disband in Phase 4).
	w.expect(w.do(http.MethodPost, lp, stay, nil), http.StatusConflict, "sole leader leaving")
}

// --- audit -----------------------------------------------------------------------------------------------------------------

func TestFactionPhase4AuditEventsAreSafe(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Audit Trail Unit", "ATU")
	member := w.players[1]
	mMember := w.joined(leader, fid, member)
	secretName := "SecretLogoName-7f3a.png"
	w.expect(w.upload(w.a1, fid, leader, secretName, "image/png", testPNG(t, 256, 256)), http.StatusCreated, "upload")
	w.expect(w.upload(w.a1, fid, leader, secretName, "image/png", testPNG(t, 300, 300)), http.StatusOK, "replace")
	w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/logo"), leader, nil), http.StatusOK, "delete")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/transfer-leadership"), leader, map[string]any{"memberId": mMember}), http.StatusOK, "transfer")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/leave"), leader, nil), http.StatusOK, "leave")

	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for _, ev := range []string{"faction_logo_uploaded", "faction_logo_replaced", "faction_logo_deleted", "faction_leadership_transferred", "faction_member_left"} {
		if !strings.Contains(logs, "event="+ev) {
			t.Errorf("missing audit event %s", ev)
		}
	}
	for _, secret := range []string{secretName, "SecretLogoName", "PNG\r\n", "IHDR", "test-secret", "Audit Trail Unit"} {
		if strings.Contains(logs, secret) {
			t.Errorf("audit logs must not contain file names, image bytes, faction names or secrets: found %q", secret)
		}
	}
	for _, id := range []string{"acting_user_id=", "faction_id=", "asset_id=", "previous_leader_user_id=", "new_leader_user_id="} {
		if !strings.Contains(logs, id) {
			t.Errorf("audit events must carry %s", id)
		}
	}
}
