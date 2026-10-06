package playercard

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math"
	"runtime"
	"sync"
)

// The animated card (docs/CHAMPION_CARD.md, "Animated card"): the still, filling in over 2.6 s at
// 25 fps, then held for 4 s, looping forever. Every element has a window in which it eases from
// its start state to its place; before the window it is at its start state, after it at its end
// state, so the frame at t = +Inf is the still itself.

const (
	// FrameRate is the animation's frames per second; a frame every frameDelay hundredths.
	FrameRate  = 25
	frameDelay = 100 / FrameRate
	// MotionFrames is how many frames the elements move for (2.6 s); the still then holds for
	// HoldDelay hundredths.
	MotionFrames = 65
	HoldDelay    = 400
)

// easeOut is the cubic ease-out: fast at first, settling gently.
func easeOut(t float64) float64 {
	t = clamp01(t)
	u := 1 - t
	return 1 - u*u*u
}

// breath rises and falls once over 0..1.
func breath(t float64) float64 { return math.Sin(math.Pi * clamp01(t)) }

// window is how far, eased, t is through the window a..b: 0 before it, 1 after it.
func window(t, a, b float64) float64 { return easeOut((t - a) / (b - a)) }

// rise is how many pixels below its place an element s of the way through its window sits, for an
// element that rises px pixels in all.
func rise(px, s float64) int { return int(math.Round(px * (1 - s))) }

// frame is the state of every animated element of the card at one moment: an opacity and a rise
// for each, the emblem's scale, the glow's strength, the figures mid-count and the light sweep's
// position. frameAt computes it for a time; stillFrame is the one the still is drawn from.
type frame struct {
	header, name, row, footer float64 // opacities, 0 to 1
	nameRise, rowRise         int     // pixels below the resting place
	emblem                    float64 // the emblem's opacity
	emblemScale               float64 // the emblem's scale about its centre
	glow                      float64 // the glow's peak opacity
	tiles                     [8]tileFrame
	figures                   figures
	rank                      rankFigures
	sweepOn                   bool
	sweep                     float64 // the light band's position, 0 to 1
}

type tileFrame struct {
	alpha float64
	rise  int
}

// stillFrame is the card with everything in place: the frame at t = +Inf.
func stillFrame(c Card) frame { return frameAt(c, math.Inf(1)) }

// The sequence, in seconds from the start.
const (
	headerIn, headerSettled   = 0.00, 0.30
	emblemIn, emblemSettled   = 0.10, 0.60
	emblemStartScale          = 1.25
	glowPeak, glowRest        = 0.55, 0.30
	glowSettled               = 1.10
	glowBreathIn, glowBreathO = 1.90, 2.50
	glowBreathDepth           = 0.12
	nameIn, nameSettled       = 0.20, 0.60
	nameRise                  = 18
	rowIn, rowSettled         = 0.45, 0.80
	rowRise                   = 12
	countIn, countSettled     = 0.60, 1.80
	tileIn, tileStagger       = 0.40, 0.07
	tileDuration              = 0.30
	tileRise                  = 14
	footerIn, footerSettled   = 0.60, 0.90
	sweepIn, sweepOut         = 2.00, 2.60
)

// frameAt is the state of the card t seconds into the animation.
func frameAt(c Card, t float64) frame {
	fr := frame{header: window(t, headerIn, headerSettled), footer: window(t, footerIn, footerSettled)}
	s := window(t, emblemIn, emblemSettled)
	fr.emblem, fr.emblemScale = s, emblemStartScale-(emblemStartScale-1)*s
	fr.glow = glowAt(t)
	s = window(t, nameIn, nameSettled)
	fr.name, fr.nameRise = s, rise(nameRise, s)
	s = window(t, rowIn, rowSettled)
	fr.row, fr.rowRise = s, rise(rowRise, s)
	for i := range fr.tiles {
		start := tileIn + tileStagger*float64(i)
		s = window(t, start, start+tileDuration)
		fr.tiles[i] = tileFrame{alpha: s, rise: rise(tileRise, s)}
	}
	e := window(t, countIn, countSettled)
	fr.figures = c.figuresAt(e)
	if c.Ranked != nil {
		fr.rank = c.Ranked.figuresAt(e)
	}
	if t >= sweepIn && t <= sweepOut {
		fr.sweepOn, fr.sweep = true, (t-sweepIn)/(sweepOut-sweepIn)
	}
	return fr
}

// glowAt is the glow's peak opacity at t: it overshoots as the emblem lands, settles, and breathes
// once before the sweep.
func glowAt(t float64) float64 {
	switch {
	case t < emblemIn:
		return 0
	case t < emblemSettled:
		return glowPeak * easeOut((t-emblemIn)/(emblemSettled-emblemIn))
	case t < glowSettled:
		return glowPeak - (glowPeak-glowRest)*easeOut((t-emblemSettled)/(glowSettled-emblemSettled))
	case t >= glowBreathIn && t < glowBreathO:
		return glowRest + glowBreathDepth*breath((t-glowBreathIn)/(glowBreathO-glowBreathIn))
	default:
		return glowRest
	}
}

// frameTimes are the moments the GIF shows: MotionFrames frames a frameDelay apart, then the still.
func frameTimes() []float64 {
	times := make([]float64, 0, MotionFrames+1)
	for i := 0; i < MotionFrames; i++ {
		times = append(times, float64(i)/FrameRate)
	}
	return append(times, math.Inf(1))
}

// RenderAnimation draws the card filling in and returns it as a looping GIF: 1200x630, one global
// palette, the motion frames at 25 fps and the still held for four seconds. Each frame after the
// first carries only the rectangle that changed, with the pixels inside it that did not change
// left transparent, and frames that change nothing lengthen the one before instead. Like Render,
// it is pure: the same card always gives the same bytes.
func RenderAnimation(c Card) ([]byte, error) {
	fonts, err := loadFonts()
	if err != nil {
		return nil, fmt.Errorf("load card fonts: %w", err)
	}

	// The palette is cut from three frames: the still, the emblem's landing (the glow at its
	// peak) and the middle of the light sweep. One entry is kept for the transparent index.
	cv := newCanvas(fonts)
	var samples []*image.RGBA
	for _, t := range []float64{math.Inf(1), emblemSettled, (sweepIn + sweepOut) / 2} {
		if err := cv.draw(c, frameAt(c, t)); err != nil {
			return nil, err
		}
		samples = append(samples, cloneRGBA(cv.img))
	}
	pal := newPalette(medianCut(histogram(samples...), maxPaletteSize-1))
	samples = nil
	transparent := uint8(len(pal.colors))
	pal.colors = append(pal.colors, color.RGBA{})

	g := &gif.GIF{LoopCount: 0, Config: image.Config{ColorModel: pal.colors, Width: Width, Height: Height}}
	times := frameTimes()
	var prev []uint8
	err = renderFrames(c, fonts, pal, times, func(i int, pix []uint8) (free []uint8) {
		delay := frameDelay
		if i == len(times)-1 {
			delay = HoldDelay
		}
		var pm *image.Paletted
		if prev == nil {
			pm = croppedPaletted(pix, Width, image.Rect(0, 0, Width, Height), pal.colors)
		} else {
			rect, ok := changedRect(prev, pix, Width, Height)
			if !ok {
				g.Delay[len(g.Delay)-1] += delay
				return pix
			}
			pm = croppedPaletted(pix, Width, rect, pal.colors)
			// What the frame leaves as it was stays as it was: those pixels are transparent,
			// which also makes them runs the encoder packs into almost nothing.
			for y := rect.Min.Y; y < rect.Max.Y; y++ {
				row := pm.Pix[(y-rect.Min.Y)*pm.Stride:][:rect.Dx()]
				for x, v := range row {
					if prev[y*Width+rect.Min.X+x] == v {
						row[x] = transparent
					}
				}
			}
		}
		g.Image = append(g.Image, pm)
		g.Delay = append(g.Delay, delay)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
		prev, free = pix, prev
		return free
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, fmt.Errorf("encode animated card: %w", err)
	}
	return buf.Bytes(), nil
}

// renderFrames draws every frame at its time, maps it to the palette and hands the index maps to
// emit in order. Frames are independent, so a few render at once on their own canvases and lookup
// tables; the outcome is the same whatever the order. emit may keep the map it is given and
// returns a map it no longer needs, if any, for the next frame to reuse.
func renderFrames(c Card, fonts *fontSet, pal *palette, times []float64, emit func(i int, pix []uint8) (free []uint8)) error {
	n := len(times)
	workers := min(runtime.NumCPU(), 4, n)
	// Each worker draws on its own three-megabyte canvas; only a few finished index maps wait
	// for the consumer at once.
	inflight := 2 * workers
	jobs := make(chan int, n)
	results := make([]chan quantized, n)
	for i := range results {
		results[i] = make(chan quantized, 1)
	}
	spare := make(chan []uint8, inflight+2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cv, m := newCanvas(fonts), newMapper(pal)
			for i := range jobs {
				var out quantized
				if out.err = cv.draw(c, frameAt(c, times[i])); out.err == nil {
					select {
					case out.pix = <-spare:
					default:
						out.pix = make([]uint8, Width*Height)
					}
					m.quantize(cv.img, out.pix)
				}
				results[i] <- out
			}
		}()
	}
	for i := 0; i < inflight && i < n; i++ {
		jobs <- i
	}
	var err error
	for i := 0; i < n; i++ {
		out := <-results[i]
		if out.err != nil {
			// The frames already queued finish on their own (every result channel has room).
			err = out.err
			break
		}
		if i+inflight < n {
			jobs <- i + inflight
		}
		if free := emit(i, out.pix); free != nil {
			select {
			case spare <- free:
			default:
			}
		}
	}
	close(jobs)
	wg.Wait()
	return err
}

type quantized struct {
	pix []uint8
	err error
}

func cloneRGBA(img *image.RGBA) *image.RGBA {
	cp := *img
	cp.Pix = append([]uint8(nil), img.Pix...)
	return &cp
}

// changedRect is the bounding rectangle of the pixels that differ between two index maps of the
// same size; ok is false when nothing differs.
func changedRect(prev, cur []uint8, w, h int) (image.Rectangle, bool) {
	x0, y0, x1, y1 := w, h, -1, -1
	for y := 0; y < h; y++ {
		a, b := prev[y*w:(y+1)*w], cur[y*w:(y+1)*w]
		if bytes.Equal(a, b) {
			continue
		}
		l := 0
		for a[l] == b[l] {
			l++
		}
		r := w - 1
		for a[r] == b[r] {
			r--
		}
		x0, x1 = min(x0, l), max(x1, r)
		y0, y1 = min(y0, y), y
	}
	if y1 < 0 {
		return image.Rectangle{}, false
	}
	return image.Rect(x0, y0, x1+1, y1+1), true
}

// croppedPaletted is the part of an index map inside r as a GIF frame.
func croppedPaletted(pix []uint8, w int, r image.Rectangle, pal color.Palette) *image.Paletted {
	pm := image.NewPaletted(r, pal)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		copy(pm.Pix[(y-r.Min.Y)*pm.Stride:], pix[y*w+r.Min.X:y*w+r.Max.X])
	}
	return pm
}
