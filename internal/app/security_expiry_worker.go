package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// When a player's paid Base Raid Alarm or Perimeter Watch runs out (with no more paid time after
// it), they get one DM saying so. It never renews or charges by itself.

type securityExpiryStore interface {
	DueExpiries(ctx context.Context, limit int) ([]repository.SecurityExpiry, error)
	MarkExpiryNotified(ctx context.Context, purchaseID int64) error
}

type securityExpiryWorker struct {
	store      securityExpiryStore
	dm         DMSenderFunc
	serverName func(int64) string
	siteURL    string
}

// DMSenderFunc sends one DM; it returns an error if Discord refuses.
type DMSenderFunc func(discordUserID string, msg *discordgo.MessageSend) error

func (w *securityExpiryWorker) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=security_market", "msg", "expiry worker panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	due, err := w.store.DueExpiries(ctx, 50)
	if err != nil {
		slog.Warn("component=security_market", "msg", "list expiries failed", "err", err.Error())
		return
	}
	for _, e := range due {
		if e.DiscordUserID != "" && w.dm != nil {
			if err := w.dm(e.DiscordUserID, securityExpiryMessage(e.ServiceID, w.server(e.ServerID), w.storeURL())); err != nil {
				slog.Warn("component=security_market", "msg", "expiry dm failed", "purchase_id", e.PurchaseID, "err", err.Error())
			}
		}
		// One attempt only: a failed DM is not retried, so nobody is spammed.
		if err := w.store.MarkExpiryNotified(ctx, e.PurchaseID); err != nil {
			slog.Warn("component=security_market", "msg", "mark expiry failed", "purchase_id", e.PurchaseID, "err", err.Error())
		}
	}
}

func (w *securityExpiryWorker) server(id int64) string {
	if w.serverName != nil {
		if n := w.serverName(id); n != "" {
			return n
		}
	}
	return "your server"
}

func (w *securityExpiryWorker) storeURL() string {
	base := strings.TrimRight(w.siteURL, "/")
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return base + "/dashboard/player/security-store"
}

func securityExpiryMessage(serviceID, serverName, storeURL string) *discordgo.MessageSend {
	what := "You won't get raid messages for your base any more."
	switch serviceID {
	case repository.ServicePerimeterWatch:
		what = "You won't get messages when someone comes near your base any more."
	case repository.ServiceBaseBlackBox:
		what = "New visits to your base won't be added to its history any more."
	case repository.ServiceFactionSecurity:
		what = "Your faction won't get your base's alerts any more."
	case repository.ServiceSentinelPro:
		what = "The base services it included have stopped, unless you bought them separately."
	}
	text := "⏰ **Your " + repository.SecurityServiceLabel(serviceID) + " on " + caseSafeName(serverName) + " has ended.** " + what + " Nothing was charged."
	if storeURL != "" {
		text += "\nWant it back? Buy it again in the Security Store: " + storeURL
	} else {
		text += "\nWant it back? Buy it again in the Security Store on the Champion website."
	}
	return &discordgo.MessageSend{Content: text, AllowedMentions: &discordgo.MessageAllowedMentions{
		Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

func caseSafeName(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("@", "@​", "`", "'", "*", "", "\n", " ").Replace(s))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	if s == "" {
		return "your server"
	}
	return s
}

// startSecurityExpiryWorker checks for ended paid alarms every 5 minutes.
func (a *App) startSecurityExpiryWorker(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	site := ""
	if a.Config != nil {
		site = a.Config.SiteBaseURL
	}
	w := &securityExpiryWorker{store: repository.NewSecurityServiceRepository(a.DB.Pool), serverName: a.serverNameFunc(), siteURL: site,
		dm: func(userID string, msg *discordgo.MessageSend) error {
			ch, err := session.UserChannelCreate(userID)
			if err != nil {
				return err
			}
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
			return err
		}}
	// One process only (singleton_leader.go): the DM is sent before the purchase is marked, so
	// two processes would both send it.
	go a.singleton(ctx, "security_expiry", func(ctx context.Context) {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	})
}
