//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestInviteReport(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	w.a.Invites = repository.NewInviteRepository(w.a.DB.Pool)
	now := time.Now().UTC()
	join := func(member, code string, at time.Time) {
		t.Helper()
		j := repository.InviteJoin{GuildDiscordID: w.f.DiscordGuildID, MemberDiscordID: member, Code: code, Attributed: code != "", JoinedAt: at}
		if code != "" {
			j.InviterDiscordID, j.InviterName = "inviter-"+code, "recruiter "+code
		}
		if err := w.a.Invites.RecordInviteJoin(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	link := func(member string, player int64) {
		t.Helper()
		if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, w.guildID, player, member); err != nil {
			t.Fatal(err)
		}
	}
	// Invite "good": three joins, two linked and playing (one of them recently), one left.
	join("m1", "good", now.AddDate(0, 0, -20))
	join("m2", "good", now.AddDate(0, 0, -10))
	join("m3", "good", now.AddDate(0, 0, -5))
	p1, p2 := w.player("One"), w.player("Two")
	link("m1", p1)
	link("m2", p2)
	w.activeOn(p1, now.AddDate(0, 0, -18), 3600, 1)
	w.activeOn(p2, now.AddDate(0, 0, -2), 3600, 1)
	if err := w.a.Invites.RecordInviteLeave(ctx, w.f.DiscordGuildID, "m3", now.AddDate(0, 0, -4)); err != nil {
		t.Fatal(err)
	}
	// Invite "meh": one join, never linked. Plus one unattributed join and one outside the window.
	join("m4", "meh", now.AddDate(0, 0, -3))
	join("m5", "", now.AddDate(0, 0, -1))
	join("m6", "good", now.AddDate(0, 0, -100))
	// A rejoin counts once, under the latest invite.
	join("m4", "good", now.AddDate(0, 0, -1))
	// Joins in a guild Champion does not know are dropped.
	if err := w.a.Invites.RecordInviteJoin(ctx, repository.InviteJoin{GuildDiscordID: "999", MemberDiscordID: "x", JoinedAt: now}); err != nil {
		t.Fatal(err)
	}

	rr := w.call(w.a.handleInviteReport, http.MethodGet, w.path("/invites?days=30"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("report: %d %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[struct {
		Report   repository.InviteReport `json:"report"`
		Tracking struct {
			Ready   bool   `json:"ready"`
			Problem string `json:"problem"`
		} `json:"tracking"`
	}](t, rr)
	got := map[string]repository.InviteStat{}
	for _, s := range body.Report.Invites {
		got[s.Code] = s
	}
	good := got["good"]
	if good.Joins != 4 || good.StillIn != 3 || good.Linked != 2 || good.Played != 2 || good.ActiveNow != 1 || good.InviterName != "recruiter good" {
		t.Fatalf("good invite: %+v", good)
	}
	if _, ok := got["meh"]; ok {
		t.Fatalf("a rejoin must count under the latest invite only: %+v", body.Report.Invites)
	}
	if got[""].Joins != 1 || body.Report.Totals.Joins != 5 {
		t.Fatalf("unattributed/totals: %+v %+v", got[""], body.Report.Totals)
	}
	if body.Tracking.Ready || body.Tracking.Problem == "" {
		t.Fatalf("with no tracker running the report must say so: %+v", body.Tracking)
	}
	if rr := w.call(w.a.handleInviteReport, http.MethodGet, w.path("/invites"), "stranger", nil, nil); rr.Code == http.StatusOK {
		t.Fatal("non-staff read invite results")
	}
}
