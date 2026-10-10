package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func TestServerStatusBoardShowsTheGameServer(t *testing.T) {
	now := time.Date(2026, 10, 10, 17, 0, 0, 0, time.UTC)
	boot := now.Add(-24 * time.Minute)
	section := func(g GameServerStatus) []ServerStatusSection {
		return []ServerStatusSection{{ServerName: "Chernarus", Game: g}}
	}

	online := BuildServerStatusEmbed(section(GameServerStatus{State: "ONLINE", StartedAt: boot, NextRestart: boot.Add(68 * time.Minute)}), now)
	text := online.Fields[0].Value
	for _, want := range []string{"🟢 Online", "**Last restart:** " + presentation.Timestamp(boot, 't'), "**Next restart:** about " + presentation.Timestamp(boot.Add(68*time.Minute), 't'), "**Champion link:**"} {
		if !strings.Contains(text, want) {
			t.Errorf("online board lacks %q:\n%s", want, text)
		}
	}
	// The text holds no clock reading of its own, so the same status renders the same a minute on.
	if later := BuildServerStatusEmbed(section(GameServerStatus{State: "ONLINE", StartedAt: boot, NextRestart: boot.Add(68 * time.Minute)}), now.Add(time.Minute)); later.Fields[0].Value != text {
		t.Error("an unchanged status must render the same text, or the message is edited every minute")
	}

	restarting := BuildServerStatusEmbed(section(GameServerStatus{State: "RESTARTING", StartedAt: boot}), now)
	if v := restarting.Fields[0].Value; !strings.Contains(v, "🔄 Restarting") || strings.Contains(v, "Next restart") || restarting.Color != presentation.Amber {
		t.Errorf("restarting board: color=%d\n%s", restarting.Color, v)
	}

	down := BuildServerStatusEmbed(section(GameServerStatus{State: "DOWN", StartedAt: boot, DownSince: now.Add(-30 * time.Minute)}), now)
	if v := down.Fields[0].Value; !strings.Contains(v, "🔴 Not running since "+presentation.Timestamp(now.Add(-30*time.Minute), 't')) || down.Color != presentation.ErrorRed {
		t.Errorf("down board: color=%d\n%s", down.Color, v)
	}

	// Nothing known adds nothing: the board reads as it did before.
	if v := BuildServerStatusEmbed(section(GameServerStatus{}), now).Fields[0].Value; strings.Contains(v, "Game server") {
		t.Errorf("unknown status must add no line:\n%s", v)
	}
}

func TestObserveGameTriggersOnlyOnChange(t *testing.T) {
	b := NewServerStatusBoard(nil, nil, nil)
	g := GameServerStatus{State: "ONLINE", StartedAt: time.Unix(1000, 0)}
	b.ObserveGame(1, g)
	select {
	case <-b.trigger:
	default:
		t.Fatal("a new status must ask for a refresh")
	}
	b.ObserveGame(1, g)
	select {
	case <-b.trigger:
		t.Fatal("the same status again must not ask for a refresh")
	default:
	}
}
