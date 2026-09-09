package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestSetupCreatesOnlinePlayersAsVoiceChannel(t *testing.T) {
	api := newFakeGuildAPI()
	store := NewInMemorySetupStore()
	m := NewSetupManager(api, store, "bot-1")

	setup, _, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if setup.OnlinePlayersChannelID == "" {
		t.Fatal("expected OnlinePlayersChannelID to be stored")
	}

	// The stored online-players channel must be a VOICE channel.
	ch := findChannel(api.channels, setup.OnlinePlayersChannelID)
	if ch == nil {
		t.Fatal("online-players channel not found in guild")
	}
	if ch.Type != discordgo.ChannelTypeGuildVoice {
		t.Fatalf("expected online-players to be a voice channel, got type %d", ch.Type)
	}
}

func TestSetupVoiceCounterPermissions(t *testing.T) {
	m := NewSetupManager(newFakeGuildAPI(), NewInMemorySetupStore(), "bot-1")
	overwrites := m.voiceCounterOverwrites("guild-9")

	var everyone, bot *discordgo.PermissionOverwrite
	for _, ow := range overwrites {
		switch ow.Type {
		case discordgo.PermissionOverwriteTypeRole:
			everyone = ow // @everyone (role ID == guild ID)
		case discordgo.PermissionOverwriteTypeMember:
			bot = ow // the bot member
		}
	}
	if everyone == nil || bot == nil {
		t.Fatal("expected both @everyone and bot overwrites")
	}
	// @everyone: view allowed, connect + speak denied.
	if everyone.Allow&discordgo.PermissionViewChannel == 0 {
		t.Error("expected @everyone ViewChannel allowed")
	}
	if everyone.Deny&discordgo.PermissionVoiceConnect == 0 {
		t.Error("expected @everyone Connect denied")
	}
	if everyone.Deny&discordgo.PermissionVoiceSpeak == 0 {
		t.Error("expected @everyone Speak denied")
	}
	// Bot: ViewChannel + ManageChannels (to rename the counter).
	if bot.Allow&discordgo.PermissionViewChannel == 0 {
		t.Error("expected bot ViewChannel allowed")
	}
	if bot.Allow&discordgo.PermissionManageChannels == 0 {
		t.Error("expected bot ManageChannels allowed")
	}
}

func TestSetupMigratesLegacyTextOnlinePlayersChannel(t *testing.T) {
	api := newFakeGuildAPI()
	store := NewInMemorySetupStore()
	m := NewSetupManager(api, store, "bot-1")

	setup, _, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Simulate a legacy TEXT online-players channel stored from an older version.
	legacyID := setup.OnlinePlayersChannelID
	legacy := findChannel(api.channels, legacyID)
	legacy.Type = discordgo.ChannelTypeGuildText // force legacy text type

	setup2, report, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if setup2.OnlinePlayersChannelID == legacyID {
		t.Fatal("expected a new voice channel ID after migration")
	}
	newCh := findChannel(api.channels, setup2.OnlinePlayersChannelID)
	if newCh == nil || newCh.Type != discordgo.ChannelTypeGuildVoice {
		t.Fatal("expected migration to create a voice channel")
	}
	migrated := false
	for _, r := range report.Repaired {
		if r == "online-players(migrated-to-voice)" {
			migrated = true
		}
	}
	if !migrated {
		t.Fatal("expected repair to report the voice migration")
	}
	// Legacy text channel must NOT be deleted automatically.
	if findChannel(api.channels, legacyID) == nil {
		t.Fatal("legacy text channel must not be auto-deleted")
	}
}
