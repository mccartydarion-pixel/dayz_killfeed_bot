//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakePanelSender struct {
	sent, edits int
	editErr     error
	lastEmbed   *discordgo.MessageEmbed
}

func (f *fakePanelSender) ChannelMessageSendComplex(_ string, m *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sent++
	f.lastEmbed = m.Embeds[0]
	return &discordgo.Message{ID: "900000000000000001"}, nil
}

func (f *fakePanelSender) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edits++
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.lastEmbed = (*m.Embeds)[0]
	return &discordgo.Message{ID: m.ID}, nil
}

func TestSecurityStorePanelPublish(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	s := repository.SecurityScope{InstallationID: w.f.InstallationID, GuildID: w.guildID, ServerID: w.serverID}
	repo := repository.NewSecurityStorePanelRepository(pool)
	if p, err := repo.Get(ctx, s); err != nil || p != nil {
		t.Fatalf("no panel yet: %+v %v", p, err)
	}
	if _, err := repo.Set(ctx, s, "not-a-channel", true, nil); err == nil {
		t.Fatal("bad channel id accepted")
	}
	p, err := repo.Set(ctx, s, "800000000000000001", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePanelSender{}
	if err := w.a.publishSecurityPanel(ctx, f, p); err != nil || f.sent != 1 || f.edits != 0 {
		t.Fatalf("first post: %v %+v", err, f)
	}
	if len(f.lastEmbed.Fields) != 0 {
		t.Fatalf("nothing on sale yet: %+v", f.lastEmbed.Fields)
	}
	// Put the raid alarm on sale; the next publish edits the same message.
	if _, err := repository.NewBaseRaidAlarmRepository(pool).SetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewSecurityServiceRepository(pool).SetOffer(ctx, s, repository.ServiceBaseRaidAlarm, true, 500, 7, nil); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.Get(ctx, s)
	if stored.MessageID != "900000000000000001" || stored.LastPostedAt == nil {
		t.Fatalf("message not recorded: %+v", stored)
	}
	if err := w.a.publishSecurityPanel(ctx, f, *stored); err != nil || f.edits != 1 || f.sent != 1 || len(f.lastEmbed.Fields) != 2 {
		t.Fatalf("edit: %v %+v %+v", err, f, f.lastEmbed.Fields)
	}
	// The message was deleted: post a new one.
	f.editErr = &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}}
	if err := w.a.publishSecurityPanel(ctx, f, *stored); err != nil || f.sent != 2 {
		t.Fatalf("repost after delete: %v %+v", err, f)
	}
	// Another failure is recorded for the owner.
	f.editErr = &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	if err := w.a.publishSecurityPanel(ctx, f, *stored); err == nil {
		t.Fatal("a forbidden edit must fail")
	}
	if got, _ := repo.Get(ctx, s); got.LastError == "" {
		t.Fatalf("error not recorded: %+v", got)
	}
	// Moving channel forgets the old message.
	moved, err := repo.Set(ctx, s, "800000000000000002", true, nil)
	if err != nil || moved.MessageID != "" || moved.LastError != "" {
		t.Fatalf("moved: %+v %v", moved, err)
	}
	if list, err := repo.Enabled(ctx); err != nil || len(list) < 1 {
		t.Fatalf("enabled list: %+v %v", list, err)
	}
	// Owner only.
	admin := zoneActor(t, w, "panel-admin")
	w.mapRole(admin, "panel-admin-role", "ADMINISTRATOR")
	if rr := w.call(w.a.handleGetSecurityPanel, http.MethodGet, w.path("/case/security-panel"), admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", rr.Code)
	}
	if rr := w.call(w.a.handleGetSecurityPanel, http.MethodGet, w.path("/case/security-panel"), w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("owner read: %d", rr.Code)
	}
}
