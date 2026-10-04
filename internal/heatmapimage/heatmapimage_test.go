package heatmapimage

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != Size || img.Bounds().Dy() != Size {
		t.Fatalf("size: %v", img.Bounds())
	}
	return img
}

func TestRenderDrawsDotsWhereTheKillsWere(t *testing.T) {
	// Chernarus, 15,360 m: NWAF (4,600 / 10,400) and Chernogorsk (6,650 / 2,600).
	out, err := Render(nil, 15360, []Point{{X: 4600, Z: 10400, Count: 9}, {X: 6650, Z: 2600, Count: 1}})
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, out)
	scale := float64(Size) / 15360
	at := func(x, z float64) color.RGBA {
		return color.RGBAModel.Convert(img.At(int(x*scale), int((15360-z)*scale))).(color.RGBA)
	}
	busy, quiet, empty := at(4600, 10400), at(6650, 2600), at(13000, 1000)
	if busy.R < 200 || busy.G > 120 {
		t.Fatalf("the busiest cell is crimson: %v", busy)
	}
	if quiet.R < 200 || quiet.G < 150 {
		t.Fatalf("a quiet cell is gold: %v", quiet)
	}
	if empty.R > 60 {
		t.Fatalf("open map stays dark: %v", empty)
	}
	if _, err := Render(nil, 0, nil); err == nil {
		t.Fatal("a zero map size is refused")
	}
	if out, err := Render(nil, 12800, nil); err != nil || len(out) == 0 {
		t.Fatal("an empty heatmap still renders the map")
	}
}

func TestTilesAreFetchedOnceAndFailuresFallBack(t *testing.T) {
	hits := 0
	tile := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range tile.Pix {
		tile.Pix[i] = 200
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, tile)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/maps/broken/2/0_0.webp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(buf.Bytes()) // PNG bytes decode as well as WebP
	}))
	defer srv.Close()
	tiles := &Tiles{BaseURL: srv.URL}
	bg := tiles.Background(context.Background(), "chernarusplus")
	if bg == nil || bg.Bounds().Dx() != Size {
		t.Fatal("background")
	}
	first := hits
	if first != 16 {
		t.Fatalf("16 tiles at level 2: %d", first)
	}
	_ = tiles.Background(context.Background(), "chernarusplus")
	if hits != first {
		t.Fatal("cached after the first fetch")
	}
	if tiles.Background(context.Background(), "broken") != nil {
		t.Fatal("a missing tile means no background")
	}
	if (&Tiles{}).Background(context.Background(), "chernarusplus") != nil {
		t.Fatal("no base URL, no background")
	}
	out, err := Render(bg, 15360, []Point{{X: 100, Z: 100, Count: 3}})
	if err != nil || len(out) == 0 {
		t.Fatal(err)
	}
}
