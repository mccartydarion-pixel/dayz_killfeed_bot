package factionassets

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/assetstore"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/logoimage"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"logo.png":                        "logo.png",
		"My Faction Logo.PNG":             "My Faction Logo.PNG",
		"../../etc/passwd":                "passwd",
		`..\..\windows\system32\cmd.exe`:  "cmd.exe",
		"a/b/c/../../evil.png":            "evil.png",
		"   spaced    name  .png ":        "spaced name .png",
		".htaccess":                       "htaccess",
		"...":                             "",
		"":                                "",
		`"><script>alert(1)</script>.png`: "script.png", // the / in </script> is a path separator
		"caf\u00e9.png":                   "caf.png",
		"logo\x00.png":                    "logo.png",
		"C:\\Users\\me\\logo.png":         "logo.png",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeFilename(strings.Repeat("a", 500) + ".png"); len(got) > 100 {
		t.Errorf("filename must be capped at 100, got %d", len(got))
	}
}

func TestNormalizeType(t *testing.T) {
	for in, want := range map[string]string{"": "", "image/png": "image/png", " IMAGE/PNG ; charset=x": "image/png", "application/octet-stream": "", "binary/octet-stream": "",
		"image/jpg": "image/jpeg", "image/pjpeg": "image/jpeg", "image/svg+xml": "image/svg+xml"} {
		if got := normalizeType(in); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewUUIDShapeAndUniqueness(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := newUUID()
		if err != nil || !re.MatchString(id) || seen[id] {
			t.Fatalf("bad or repeated uuid %q (%v)", id, err)
		}
		seen[id] = true
	}
}

// fakeRepo is an in-memory Repo that lets the ordering and failure handling be tested exactly.
type fakeRepo struct {
	mu         sync.Mutex
	current    *factionhub.Asset
	replaceEr  error
	deleteEr   error
	events     []string
	referenced map[string]bool
	byPublic   map[string]*factionhub.Asset
}

func (f *fakeRepo) RequireLeader(context.Context, int64, int64, int64, int64) error { return nil }
func (f *fakeRepo) ReplaceLogo(_ context.Context, _, _, faction, _ int64, in repository.NewLogoAsset) (factionhub.Asset, *factionhub.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "replace:"+in.StorageKey)
	if f.replaceEr != nil {
		return factionhub.Asset{}, nil, f.replaceEr
	}
	old := f.current
	a := factionhub.Asset{ID: 1, PublicID: in.PublicID, FactionID: faction, StorageKey: in.StorageKey, ContentType: in.ContentType, SizeBytes: in.SizeBytes, Width: in.Width, Height: in.Height, OriginalFilename: in.OriginalFilename}
	f.current = &a
	return a, old, nil
}
func (f *fakeRepo) DeleteLogo(context.Context, int64, int64, int64, int64) (*factionhub.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteEr != nil {
		return nil, f.deleteEr
	}
	old := f.current
	f.current = nil
	return old, nil
}
func (f *fakeRepo) AssetByPublicID(_ context.Context, id string) (*factionhub.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.byPublic[id]; a != nil {
		return a, nil
	}
	return nil, factionhub.ErrNotFound
}
func (f *fakeRepo) ReferencedAssetKeys(_ context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, k := range keys {
		if f.referenced[k] {
			out[k] = true
		}
	}
	return out, nil
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 7, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func upload(s *Service, data []byte, declared string) (UploadResult, error) {
	return s.UploadLogo(context.Background(), UploadInput{OrganizationID: 7, InstallationID: 8, FactionID: 9, ActorUserID: 1, Data: data, DeclaredType: declared, Filename: "../x.png"})
}

func TestUploadOrderNewBytesBeforeMetadataOldBytesAfter(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{}
	s := NewService(store, repo)

	first, err := upload(s, pngBytes(t, 200, 200), "image/png")
	if err != nil || first.Replaced {
		t.Fatalf("first: %+v %v", first, err)
	}
	if !store.Has(first.Asset.StorageKey) || store.Len() != 1 {
		t.Fatal("first bytes must be stored")
	}
	if want := "factions/7/8/9/" + first.Asset.PublicID + ".png"; first.Asset.StorageKey != want {
		t.Fatalf("storage key %q, want %q", first.Asset.StorageKey, want)
	}
	if first.Asset.OriginalFilename != "x.png" {
		t.Fatalf("filename must be sanitized: %q", first.Asset.OriginalFilename)
	}

	// While the repo runs the replacement, BOTH the old and the new bytes must exist in the store
	// (the old logo is still live until the commit).
	repo.replaceEr = nil
	second, err := upload(s, pngBytes(t, 220, 220), "")
	if err != nil || !second.Replaced {
		t.Fatalf("second: %+v %v", second, err)
	}
	if store.Has(first.Asset.StorageKey) || !store.Has(second.Asset.StorageKey) || store.Len() != 1 {
		t.Fatalf("after replacement only the new bytes remain (len=%d)", store.Len())
	}
}

func TestUploadRollsBackBytesWhenMetadataFails(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{}
	s := NewService(store, repo)
	repo.replaceEr = factionhub.ErrForbidden
	if _, err := upload(s, pngBytes(t, 200, 200), "image/png"); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if store.Len() != 0 {
		t.Fatalf("the bytes written before the failed transaction must be deleted, %d left", store.Len())
	}
	// If that cleanup also fails, the object is left for the sweep (never a hard failure).
	store.DeleteErr = errors.New("nope")
	if _, err := upload(s, pngBytes(t, 200, 200), "image/png"); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("an object whose cleanup failed stays as an orphan, got %d", store.Len())
	}
	store.DeleteErr = nil
	store.AgeAll(3 * time.Hour)
	if n, err := s.SweepOrphans(context.Background(), time.Hour, 10, 2); err != nil || n != 1 || store.Len() != 0 {
		t.Fatalf("sweep: %d %v left=%d", n, err, store.Len())
	}
}

func TestUploadStoreFailureRecordsNothing(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{}
	store.PutErr = errors.New("bucket unreachable")
	s := NewService(store, repo)
	_, err := upload(s, pngBytes(t, 200, 200), "image/png")
	if !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "bucket") {
		t.Fatalf("got %v", err)
	}
	if len(repo.events) != 0 {
		t.Fatalf("no metadata may be written when the bytes could not be stored: %v", repo.events)
	}
}

func TestUploadValidationHappensBeforeAnyStorage(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{}
	s := NewService(store, repo)
	for name, tc := range map[string]struct {
		data     []byte
		declared string
		want     error
	}{
		"svg":           {[]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), "image/svg+xml", logoimage.ErrUnsupported},
		"empty":         {nil, "image/png", logoimage.ErrEmpty},
		"too small":     {pngBytes(t, 50, 50), "image/png", logoimage.ErrDimensions},
		"type mismatch": {pngBytes(t, 200, 200), "image/jpeg", ErrTypeMismatch},
		"svg declared":  {pngBytes(t, 200, 200), "image/svg+xml", ErrTypeMismatch},
	} {
		if _, err := upload(s, tc.data, tc.declared); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
	}
	if store.Len() != 0 || len(repo.events) != 0 {
		t.Fatalf("rejected uploads must touch neither store nor repo (%d objects, %v)", store.Len(), repo.events)
	}
}

func TestDeleteLogoIsIdempotentAndRemovesBytes(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{}
	s := NewService(store, repo)
	res, err := upload(s, pngBytes(t, 200, 200), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.DeleteLogo(context.Background(), 7, 8, 9, 1)
	if err != nil || old == nil || old.StorageKey != res.Asset.StorageKey || store.Len() != 0 {
		t.Fatalf("delete: %+v %v left=%d", old, err, store.Len())
	}
	if again, err := s.DeleteLogo(context.Background(), 7, 8, 9, 1); err != nil || again != nil {
		t.Fatalf("second delete: %+v %v", again, err)
	}
	repo.deleteEr = factionhub.ErrForbidden
	if _, err := s.DeleteLogo(context.Background(), 7, 8, 9, 2); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("authorization failure must propagate: %v", err)
	}
}

func TestSweepNeverDeletesReferencedOrFreshObjects(t *testing.T) {
	store, repo := assetstore.NewMemoryStore(), &fakeRepo{referenced: map[string]bool{"factions/1/1/1/live.png": true}}
	s := NewService(store, repo)
	ctx := context.Background()
	_ = store.Put(ctx, "factions/1/1/1/live.png", "image/png", []byte("1"))
	_ = store.Put(ctx, "factions/1/1/1/orphan.png", "image/png", []byte("1"))
	_ = store.Put(ctx, "other/1/x.png", "image/png", []byte("1")) // not ours
	if n, _ := s.SweepOrphans(ctx, time.Hour, 10, 2); n != 0 {
		t.Fatalf("fresh objects are never swept, got %d", n)
	}
	store.AgeAll(2 * time.Hour)
	if n, err := s.SweepOrphans(ctx, time.Hour, 10, 2); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if !store.Has("factions/1/1/1/live.png") || store.Has("factions/1/1/1/orphan.png") || !store.Has("other/1/x.png") {
		t.Fatal("only the unreferenced factions/ object may go")
	}
}

func TestServeUsesRecordedTypeAndHidesInconsistency(t *testing.T) {
	store := assetstore.NewMemoryStore()
	repo := &fakeRepo{byPublic: map[string]*factionhub.Asset{}}
	s := NewService(store, repo)
	res, err := upload(s, pngBytes(t, 200, 200), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	repo.byPublic[res.Asset.PublicID] = &res.Asset
	served, err := s.Serve(context.Background(), res.Asset.PublicID)
	if err != nil || served.ContentType != "image/png" || len(served.Data) == 0 {
		t.Fatalf("serve: %+v %v", served, err)
	}
	if _, err := s.Serve(context.Background(), "00000000-0000-4000-8000-000000000000"); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	_ = store.Delete(context.Background(), res.Asset.StorageKey) // metadata without bytes
	if _, err := s.Serve(context.Background(), res.Asset.PublicID); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("missing bytes must look like not found, got %v", err)
	}
}
