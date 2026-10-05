package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Map pictures (docs/MAP_ROTATION.md, "The picture"): what an upload must be, what the views say
// about it and how the stored bytes are served. No database here; the round trip through
// PostgreSQL is in map_rotation_integration_test.go.

// Minimal files that carry the signature of their type, padded to size bytes.
func padTo(head []byte, size int) []byte {
	if size < len(head) {
		size = len(head)
	}
	return append(append([]byte{}, head...), bytes.Repeat([]byte{0x5a}, size-len(head))...)
}

func mapTestJPEG(size int) []byte {
	return padTo([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, size)
}

func mapTestPNG(size int) []byte {
	return padTo([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D, 'I', 'H', 'D', 'R'}, size)
}

func mapTestWebP(size int) []byte {
	return padTo([]byte{'R', 'I', 'F', 'F', 0x24, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}, size)
}

func b64(b []byte) *string { return str(base64.StdEncoding.EncodeToString(b)) }

func imageVersionOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func bodyWithImage(data *string, remove bool) mapRotationBody {
	b := validBody()
	(*b.Maps)[1].Name = "Dust"
	(*b.Maps)[1].ImageData = data
	(*b.Maps)[1].RemoveImage = remove
	return b
}

func TestValidateMapImageAccepted(t *testing.T) {
	for want, data := range map[string][]byte{
		"image/jpeg": mapTestJPEG(64),
		"image/png":  mapTestPNG(64),
		"image/webp": mapTestWebP(64),
	} {
		in, problem := validateMapRotationBody(bodyWithImage(b64(data), false))
		if problem != "" {
			t.Fatalf("%s: %q", want, problem)
		}
		m := in.Maps[1]
		if !bytes.Equal(m.ImageData, data) || m.ImageType != want || m.ImageVersion != imageVersionOf(data) || len(m.ImageVersion) != 16 || m.RemoveImage {
			t.Fatalf("%s: stored as %q version %q (%d bytes)", want, m.ImageType, m.ImageVersion, len(m.ImageData))
		}
		// The other map sent no picture: nothing to store, nothing to remove.
		if o := in.Maps[0]; o.ImageData != nil || o.ImageType != "" || o.ImageVersion != "" || o.RemoveImage {
			t.Fatalf("a map without imageData carries a picture: %+v", o)
		}
	}
	// Exactly the limit is accepted.
	if _, problem := validateMapRotationBody(bodyWithImage(b64(mapTestPNG(MaxMapImageBytes)), false)); problem != "" {
		t.Fatalf("a picture of exactly the limit: %q", problem)
	}
	if MaxMapImageBytes != 400<<10 {
		t.Fatalf("MaxMapImageBytes = %d", MaxMapImageBytes)
	}
}

func TestValidateMapImageRefused(t *testing.T) {
	big := "Map 2 (Dust): the picture is too large (at most 400 KB)"
	kind := "Map 2 (Dust): the picture must be a JPEG, PNG or WebP image"
	unreadable := "Map 2 (Dust): the picture could not be read; upload it again"
	riffNotWebP := padTo([]byte{'R', 'I', 'F', 'F', 0x24, 0, 0, 0, 'W', 'A', 'V', 'E', 'f', 'm', 't', ' '}, 64)
	cases := map[string]struct {
		data *string
		want string
	}{
		"one byte too large":   {b64(mapTestJPEG(MaxMapImageBytes + 1)), big},
		"far too large":        {b64(mapTestPNG(2 * MaxMapImageBytes)), big},
		"svg":                  {b64([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`)), kind},
		"svg with xml header":  {b64([]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)), kind},
		"gif":                  {b64(padTo([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00"), 64)), kind},
		"plain text":           {b64([]byte("this is not a picture at all, just words")), kind},
		"html":                 {b64([]byte("<!DOCTYPE html><html><body>hi</body></html>")), kind},
		"bmp":                  {b64(padTo([]byte("BM"), 64)), kind},
		"riff that is no webp": {b64(riffNotWebP), kind},
		"not base64":           {str("this is !!! not base64"), unreadable},
		"url-safe base64":      {str(base64.URLEncoding.EncodeToString(append(mapTestJPEG(3), 0xFB, 0xFF, 0xFE))), unreadable},
		"data url":             {str("data:image/png;base64," + *b64(mapTestPNG(32))), unreadable},
	}
	for name, c := range cases {
		in, problem := validateMapRotationBody(bodyWithImage(c.data, false))
		if problem != c.want {
			t.Errorf("%s: got %q, want %q", name, problem, c.want)
		}
		if len(in.Maps) > 1 {
			t.Errorf("%s: the refused map was added to the save", name)
		}
	}
}

// Left out, null or empty, the stored picture is kept: the save carries no bytes and no removal.
func TestValidateMapImageKeptAndRemoved(t *testing.T) {
	for name, data := range map[string]*string{"absent": nil, "empty": str("")} {
		in, problem := validateMapRotationBody(bodyWithImage(data, false))
		if problem != "" || in.Maps[1].ImageData != nil || in.Maps[1].RemoveImage || in.Maps[1].ImageVersion != "" {
			t.Fatalf("%s: %q %+v", name, problem, in.Maps[1])
		}
	}
	in, problem := validateMapRotationBody(bodyWithImage(nil, true))
	if problem != "" || !in.Maps[1].RemoveImage || in.Maps[1].ImageData != nil || in.Maps[0].RemoveImage {
		t.Fatalf("removeImage: %q %+v", problem, in.Maps)
	}
	// The fields come from JSON under the contract's names; imageUrl keeps working beside them.
	var body mapRotationBody
	raw := `{"enabled":false,"everyRestarts":1,"order":"SEQUENCE","voteMinutesBeforeRestart":30,"maps":[
{"name":"A","mapFile":"a.json","spawnFile":"a.xml","imageUrl":"https://cdn.example.com/a.png","imageData":"` + *b64(mapTestWebP(40)) + `","enabled":true},
{"name":"B","mapFile":"b.json","spawnFile":"b.xml","removeImage":true,"enabled":true}]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	in, problem = validateMapRotationBody(body)
	if problem != "" || in.Maps[0].ImageType != "image/webp" || in.Maps[0].ImageURL == nil || *in.Maps[0].ImageURL != "https://cdn.example.com/a.png" || !in.Maps[1].RemoveImage || in.Maps[1].ImageData != nil {
		t.Fatalf("from JSON: %q %+v", problem, in.Maps)
	}
}

// The views say whether a picture is stored, how large and which version - never the bytes.
func TestMapImageViewFields(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	version := "0123456789abcdef"
	snap := repository.MapRotationSnapshot{
		Settings: repository.MapRotationSettings{Enabled: true, EveryRestarts: 1, Order: "SEQUENCE", VoteEnabled: true, VoteMinutes: 30, Phase: "VOTING"},
		Maps: []repository.MapRotationMap{
			{ID: 1, Name: "Arena 1", MapFile: "a.json", SpawnFile: "a.xml", SpawnBytes: 10, ImageURL: str("https://cdn.example.com/a.png"), ImageBytes: 5120, ImageVersion: &version, Enabled: true},
			{ID: 2, Name: "Arena 2", MapFile: "b.json", SpawnFile: "b.xml", SpawnBytes: 10, Enabled: true, Position: 1},
		},
		Vote: &repository.MapRotationVote{ID: 9, Status: "OPEN", OpensAt: now, ClosesAt: now.Add(time.Hour), Options: []repository.MapRotationVoteOption{
			{MapID: 1, Name: "Arena 1", ImageURL: str("https://cdn.example.com/a.png"), ImageVersion: &version},
			{MapID: 2, Name: "Arena 2", Position: 1},
		}},
	}
	raw, _ := json.Marshal(toMapRotationAdminDTO(snap, "", now))
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	m1, m2 := v["maps"].([]any)[0].(map[string]any), v["maps"].([]any)[1].(map[string]any)
	if m1["imageUploaded"] != true || m1["imageBytes"].(float64) != 5120 || m1["imageVersion"] != version || m1["imageUrl"] != "https://cdn.example.com/a.png" {
		t.Fatalf("a map with a picture: %v", m1)
	}
	if v, has := m2["imageVersion"]; m2["imageUploaded"] != false || m2["imageBytes"].(float64) != 0 || !has || v != nil {
		t.Fatalf("a map without a picture: %v", m2)
	}
	for _, m := range []map[string]any{m1, m2} {
		if _, has := m["imageData"]; has || len(m) != 12 {
			t.Fatalf("a map entry: %v", m)
		}
	}
	check := func(what string, options []any) {
		t.Helper()
		o1, o2 := options[0].(map[string]any), options[1].(map[string]any)
		if o1["imageVersion"] != version || o1["imageUrl"] != "https://cdn.example.com/a.png" || len(o1) != 5 {
			t.Fatalf("%s option with a picture: %v", what, o1)
		}
		if v, has := o2["imageVersion"]; !has || v != nil || o2["imageUrl"] != nil {
			t.Fatalf("%s option without a picture: %v", what, o2)
		}
	}
	check("admin", v["vote"].(map[string]any)["options"].([]any))
	// The player's vote is the same Vote object with myVote.
	raw, _ = json.Marshal(mapPlayerVoteDTO{mapVoteDTO: *toMapVoteDTO(snap.Vote)})
	var pv map[string]any
	_ = json.Unmarshal(raw, &pv)
	check("player", pv["options"].([]any))
}

func TestPublicMapImageRoute(t *testing.T) {
	const secret = "image-test-secret"
	data := mapTestPNG(300)
	version := imageVersionOf(data)
	var failing bool
	a := &App{Config: &config.Config{WebsiteAPISecret: secret}}
	// Installation 7 owns map 3 (with a picture) and map 4 (without); installation 8 owns map 5.
	a.mapRotationImageFor = func(ctx context.Context, installationID, mapID int64) (*repository.MapImage, error) {
		if _, bounded := ctx.Deadline(); !bounded {
			t.Error("the lookup runs without a timeout")
		}
		if failing {
			return nil, errors.New("database is down")
		}
		if installationID == 7 && mapID == 3 {
			return &repository.MapImage{Data: data, ContentType: "image/png", Version: version}, nil
		}
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/network/servers/{installationID}/map-images/{mapID}", a.handlePublicMapImage)
	get := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	const base = "/api/saas/network/servers/"

	rr := get(base+"7/map-images/3", secret)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("stored picture: %d, %d bytes", rr.Code, rr.Body.Len())
	}
	for header, want := range map[string]string{
		"Content-Type":           "image/png",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "public, max-age=31536000, immutable",
		"ETag":                   `"` + version + `"`,
		"Content-Length":         "300",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// No acting user is needed, but the service secret is.
	for _, bearer := range []string{"", "wrong"} {
		if rr := get(base+"7/map-images/3", bearer); rr.Code != http.StatusUnauthorized || bytes.Contains(rr.Body.Bytes(), data[:8]) {
			t.Fatalf("bearer %q: %d", bearer, rr.Code)
		}
	}
	notFound := map[string]string{
		"map without a picture":                   base + "7/map-images/4",
		"map of another installation":             base + "7/map-images/5",
		"picture asked of the wrong installation": base + "8/map-images/3",
		"unknown map":                             base + "7/map-images/999",
		"unknown installation":                    base + "999/map-images/3",
	}
	for name, path := range notFound {
		rr := get(path, secret)
		var body apiErrorEnvelope
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if rr.Code != http.StatusNotFound || body.Error.Code != codeNotFound || body.Error.Message == "" || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") == "public, max-age=31536000, immutable" {
			t.Errorf("%s: a 404 must not be cached for good", name)
		}
	}
	for _, path := range []string{base + "abc/map-images/3", base + "7/map-images/abc", base + "7/map-images/0", base + "7/map-images/-1"} {
		if rr := get(path, secret); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, rr.Code)
		}
	}
	failing = true
	if rr := get(base+"7/map-images/3", secret); rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "database is down") {
		t.Fatalf("a failed lookup: %d %s", rr.Code, rr.Body.String())
	}
	// Without the lookup and without the repository the route answers, it does not panic.
	if rr := func() *httptest.ResponseRecorder {
		b := &App{Config: &config.Config{WebsiteAPISecret: secret}}
		req := httptest.NewRequest(http.MethodGet, base+"7/map-images/3", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.SetPathValue("installationID", "7")
		req.SetPathValue("mapID", "3")
		rr := httptest.NewRecorder()
		b.handlePublicMapImage(rr, req)
		return rr
	}(); rr.Code != http.StatusInternalServerError {
		t.Fatalf("no repository: %d", rr.Code)
	}
}
