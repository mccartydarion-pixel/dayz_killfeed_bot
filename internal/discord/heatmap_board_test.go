package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
)

// heatmapFake serves fixed Phase 5 results per server and records queries.
type heatmapFake struct {
	mu      sync.Mutex
	results map[int64]*heatmap.Result
	err     error
	queries []heatmap.Request
}

func (f *heatmapFake) Query(_ context.Context, req heatmap.Request) (*heatmap.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, req)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.results[req.ServerID]; ok {
		return r, nil
	}
	return &heatmap.Result{Type: req.Type, Resolution: req.Resolution, Cells: []heatmap.Cell{}}, nil
}

func (f *heatmapFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func newHeatmapBoardFixture(t *testing.T) (*routedFixture, *heatmapFake, *HeatmapBoard, *time.Time) {
	t.Helper()
	f := newRoutedFixture(t)
	hm := &heatmapFake{results: map[int64]*heatmap.Result{}}
	clock := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	b := NewHeatmapBoard(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, f.panels, hm, 30*time.Minute)
	b.now = func() time.Time { return clock }
	return f, hm, b, &clock
}

func embedText(e *discordgo.MessageEmbed) string {
	var sb strings.Builder
	sb.WriteString(e.Title + "\n" + e.Description + "\n")
	for _, f := range e.Fields {
		sb.WriteString(f.Name + ": " + f.Value + "\n")
	}
	if e.Footer != nil {
		sb.WriteString(e.Footer.Text)
	}
	return sb.String()
}

func TestHeatmapSummaryUsesRealAggregatesOnly(t *testing.T) {
	res := &heatmap.Result{TotalEvents: 247, Cells: []heatmap.Cell{
		{CellX: 18, CellZ: 13, CenterX: 4625, CenterZ: 3375, Count: 21},
		{CellX: 29, CellZ: 38, CenterX: 7375, CenterZ: 9625, Count: 32},
		{CellX: 42, CellZ: 13, CenterX: 10625, CenterZ: 3375, Count: 17},
		{CellX: 1, CellZ: 1, CenterX: 375, CenterZ: 375, Count: 1},
	}}
	text := embedText(BuildHeatmapSummaryEmbed([]heatmapSection{{Result: res}}, 24*time.Hour, 250, time.Now()))
	for _, want := range []string{"PVP HEATMAP UPDATE", "Window: Last 24 Hours", "Type: PvP Kills", "Resolution: 250m", "Activity: 247 Events",
		"`#1` X: 7,375 • Z: 9,625 — **32 Kills**", "`#2` X: 4,625 • Z: 3,375 — **21 Kills**", "`#3` X: 10,625 • Z: 3,375 — **17 Kills**", "CHAMPION • LIVE SERVER INTELLIGENCE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "#4") || strings.Contains(text, "X: 375") {
		t.Fatalf("only the top three hot zones are listed:\n%s", text)
	}

	empty := embedText(BuildHeatmapSummaryEmbed([]heatmapSection{{Result: &heatmap.Result{Cells: []heatmap.Cell{}}}}, 24*time.Hour, 250, time.Now()))
	if !strings.Contains(empty, "Activity: 0 Events") || !strings.Contains(empty, "No PvP kills recorded") || strings.Contains(empty, "#1") {
		t.Fatalf("zero activity must be reported as zero, never invented:\n%s", empty)
	}
}

func TestHeatmapBoardOneMessagePerChannelEditedInPlace(t *testing.T) {
	f, hm, b, clock := newHeatmapBoardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyHeatmaps, "chan-heat")
	f.resolver.set(f.guild, 2, routeKeyHeatmaps, "chan-heat")
	hm.results[1] = &heatmap.Result{TotalEvents: 3, Cells: []heatmap.Cell{{CenterX: 125, CenterZ: 125, Count: 3}}}
	ctx := context.Background()

	b.SyncOnce(ctx)
	if f.api.sendCount() != 1 || f.api.liveIn("chan-heat") != 1 {
		t.Fatalf("want one summary message, live=%v", f.api.live())
	}
	for _, q := range hm.queries {
		if q.Type != heatmap.TypePvPKills || q.Resolution != 250 || q.To.Sub(q.From) != 24*time.Hour {
			t.Fatalf("query must be the Phase 5 24h PvP kill aggregate at 250m, got %+v", q)
		}
	}

	// Route-check ticks inside the interval do not query or post.
	queries := hm.count()
	*clock = clock.Add(10 * time.Minute)
	b.sync(ctx, false)
	if hm.count() != queries || f.api.sendCount() != 1 {
		t.Fatal("no refresh before the interval elapses")
	}

	// After the interval: refreshed by editing the same message.
	*clock = clock.Add(25 * time.Minute)
	b.sync(ctx, false)
	if hm.count() == queries {
		t.Fatal("expected a refresh once the interval elapsed")
	}
	if f.api.sendCount() != 1 || f.api.liveIn("chan-heat") != 1 {
		t.Fatalf("a refresh must edit in place, never post again, live=%v", f.api.live())
	}

	// A restart (new board, same durable store) edits the same message.
	b2 := NewHeatmapBoard(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, NewRoutePanels(f.api, f.store), hm, 30*time.Minute)
	b2.SyncOnce(ctx)
	if f.api.sendCount() != 1 || f.api.liveIn("chan-heat") != 1 {
		t.Fatalf("restart must not duplicate the summary, live=%v", f.api.live())
	}
}

func TestHeatmapBoardRouteChangeMovesSummaryImmediately(t *testing.T) {
	f, _, b, _ := newHeatmapBoardFixture(t)
	ctx := context.Background()
	f.resolver.set(f.guild, 1, routeKeyHeatmaps, "chan-old")
	b.SyncOnce(ctx)
	f.resolver.set(f.guild, 1, routeKeyHeatmaps, "chan-new")
	b.sync(ctx, false) // inside the interval, but the routes changed
	if f.api.liveIn("chan-new") != 1 || f.api.liveIn("chan-old") != 0 {
		t.Fatalf("summary must follow the route and leave no copy, live=%v", f.api.live())
	}
}

func TestHeatmapBoardNoRouteNoMessageAndQueryErrorKeepsLastGood(t *testing.T) {
	f, hm, b, clock := newHeatmapBoardFixture(t)
	ctx := context.Background()
	b.SyncOnce(ctx)
	if f.api.sendCount() != 0 || hm.count() != 0 {
		t.Fatal("no HEATMAPS route: nothing is queried or posted")
	}

	f.resolver.set(f.guild, 1, routeKeyHeatmaps, "chan-heat")
	b.SyncOnce(ctx)
	hm.err = errors.New("db down")
	*clock = clock.Add(time.Hour)
	b.sync(ctx, false)
	if f.api.liveIn("chan-heat") != 1 {
		t.Fatalf("a failed aggregate must leave the last good summary in place, live=%v", f.api.live())
	}
}

func TestHeatmapBoardNilSafe(t *testing.T) {
	var b *HeatmapBoard
	b.Trigger()
	b.SyncOnce(context.Background())
	b.SetServerNames(nil)
	NewHeatmapBoard(nil, nil, nil, nil, time.Minute).SyncOnce(context.Background())
}
