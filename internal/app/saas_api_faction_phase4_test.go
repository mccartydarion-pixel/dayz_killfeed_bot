package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

func TestSplitAssetFile(t *testing.T) {
	const id = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	for _, ext := range []string{"png", "jpg", "webp"} {
		if gotID, gotExt, ok := splitAssetFile(id + "." + ext); !ok || gotID != id || gotExt != ext {
			t.Errorf("%s.%s must parse", id, ext)
		}
	}
	bad := []string{
		"", id, id + ".", id + ".svg", id + ".gif", id + ".PNG", id + ".png.png", id + "png", "." + id + ".png",
		"../" + id + ".png", id + "/../x.png", strings.ToUpper(id) + ".png", "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4.png", // one char short
		"0a1b2c3d_4e5f_4a6b_8c7d_9e0f1a2b3c4d.png", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz.png", id + "\x00.png", "%2e%2e%2f" + id + ".png",
		"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d0.png",
	}
	for _, in := range bad {
		if _, _, ok := splitAssetFile(in); ok {
			t.Errorf("splitAssetFile(%q) must be rejected", in)
		}
	}
}

func TestToFactionLogoNeverExposesStorageDetail(t *testing.T) {
	if toFactionLogo(nil, "https://x.example") != nil {
		t.Fatal("no asset means a null logo")
	}
	a := &factionhub.Asset{ID: 5, PublicID: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", StorageKey: "factions/1/2/3/secret-key.webp", ContentType: "image/webp", Width: 512, Height: 256, OriginalFilename: "private name.png"}
	got := toFactionLogo(a, "https://api.example")
	if got.ID != 5 || got.ContentType != "image/webp" || got.Width != 512 || got.Height != 256 {
		t.Fatalf("logo: %+v", got)
	}
	if got.URL != "https://api.example/assets/faction-logos/0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d.webp" {
		t.Fatalf("url: %s", got.URL)
	}
	if strings.Contains(got.URL, "secret-key") || strings.Contains(got.URL, "private") {
		t.Fatal("the URL must not contain the storage key or the original filename")
	}
	if rel := toFactionLogo(a, ""); !strings.HasPrefix(rel.URL, "/assets/faction-logos/") {
		t.Fatalf("without a public base the URL is root-relative: %s", rel.URL)
	}
}

func TestAssetBaseURL(t *testing.T) {
	var nilApp *App
	if nilApp.assetBaseURL() != "" || (&App{}).assetBaseURL() != "" {
		t.Fatal("no config, no base")
	}
	a := &App{Config: &config.Config{PublicBaseURL: "https://api.example/"}}
	if a.assetBaseURL() != "https://api.example" {
		t.Fatalf("got %q", a.assetBaseURL())
	}
}

func TestPhase4ErrorCodesMapToStatuses(t *testing.T) {
	if httpStatusForCode[codeUnsupportedMediaType] != http.StatusUnsupportedMediaType {
		t.Fatal("UNSUPPORTED_MEDIA_TYPE must be 415")
	}
	if httpStatusForCode[codeLeadershipTransfer] != http.StatusConflict {
		t.Fatal("LEADERSHIP_TRANSFER_REQUIRED must be 409")
	}
	if codeLeadershipTransfer != "LEADERSHIP_TRANSFER_REQUIRED" || codeUnsupportedMediaType != "UNSUPPORTED_MEDIA_TYPE" {
		t.Fatal("error codes are part of the website contract")
	}
}
