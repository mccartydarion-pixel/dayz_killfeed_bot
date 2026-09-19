package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// VerifiedPlayerID extracts the player from a link record ONLY when the link is
// verified. A pending or rejected link also carries a PlayerID (the player the
// user asked to link), so using the id alone would let anyone read another
// player's private balance by merely requesting a link to them.
func VerifiedPlayerID(rec *linking.LinkRecord) (playerID int64, ok bool) {
	if rec == nil || rec.PlayerID == 0 || rec.Status != linking.StatusVerified {
		return 0, false
	}
	return rec.PlayerID, true
}

// PlayerLinks resolves a Discord user to the player they linked through the
// existing account-linking system (linking.LinkVerificationService), verified
// links only. The economy
// commands and panel buttons use it - there is no second identity system.
type PlayerLinks interface {
	LinkedPlayerID(ctx context.Context, guildRowID int64, discordUserID string) (playerID int64, linked bool)
}

// EconomyCommandHandler serves /economy:
//
//	balance [player]      own balance (via the linked account); another player's: admin only
//	history [player]      own recent transactions; another player's: admin only
//	credit  player amount [reason]   admin only
//	debit   player amount [reason]   admin only
//
// Every reply is ephemeral. Balances and histories are private: a player only ever
// sees their own; the public ECONOMY feed is separate and shows no balances beyond
// rewards. Admin means Administrator or Manage Server, as for the other admin
// commands. The acting admin is recorded on the ledger row (internal audit) and is
// never shown publicly.
type EconomyCommandHandler struct {
	economy *economy.Service
	players *repository.PlayerRepository
	guilds  GuildStore
	links   PlayerLinks
}

func NewEconomyCommandHandler(svc *economy.Service, players *repository.PlayerRepository, guilds GuildStore, links PlayerLinks) *EconomyCommandHandler {
	return &EconomyCommandHandler{economy: svc, players: players, guilds: guilds, links: links}
}

func RegisterEconomyCommands(session *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	str := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Name: name, Description: desc, Type: discordgo.ApplicationCommandOptionString, Required: required}
	}
	sub := func(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Name: name, Description: desc, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts}
	}
	amount := &discordgo.ApplicationCommandOption{Name: "amount", Description: "Champion Points", Type: discordgo.ApplicationCommandOptionInteger, Required: true, MinValue: floatPtr(1)}
	cmd := &discordgo.ApplicationCommand{Name: "economy", Description: "Champion Points balance and history", Options: []*discordgo.ApplicationCommandOption{
		sub("balance", "Show a balance (yours, or - for admins - another player's)", str("player", "Player name (admins only)", false)),
		sub("history", "Show recent transactions (yours, or - for admins - another player's)", str("player", "Player name (admins only)", false)),
		sub("credit", "Admin: add points to a player", str("player", "Player name", true), amount, str("reason", "Reason (kept in the audit log)", false)),
		sub("debit", "Admin: remove points from a player", str("player", "Player name", true), amount, str("reason", "Reason (kept in the audit log)", false)),
	}}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

func floatPtr(v float64) *float64 { return &v }

// Handle dispatches /economy. The decision logic lives in dispatch (which returns
// the reply text) so it can be exercised without a Discord session.
func (h *EconomyCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.economy == nil || h.players == nil || h.guilds == nil || i == nil || i.Member == nil || i.Member.User == nil {
		respondEphemeral(s, i, "The economy is unavailable.")
		return
	}
	ctx := context.Background()
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		respondEphemeral(s, i, "Choose a subcommand.")
		return
	}
	respondEphemeral(s, i, h.dispatch(ctx, i, guildRowID, data.Options[0]))
}

// dispatch runs one subcommand and returns the (always ephemeral) reply. credit
// and debit are admin-only and refuse before touching anything else.
func (h *EconomyCommandHandler) dispatch(ctx context.Context, i *discordgo.InteractionCreate, guildRowID int64, sub *discordgo.ApplicationCommandInteractionDataOption) string {
	switch sub.Name {
	case "balance", "history":
		playerID, msg := h.resolveSelfOrTarget(ctx, i, guildRowID, optionString(sub, "player"))
		if playerID == 0 {
			return msg
		}
		if sub.Name == "balance" {
			return h.balanceMessage(ctx, guildRowID, playerID)
		}
		return h.historyMessage(ctx, guildRowID, playerID)
	case "credit", "debit":
		if !isAdminInteraction(i) {
			return "Administrator or Manage Server permission required."
		}
		return h.adjust(ctx, i, guildRowID, sub, sub.Name == "credit")
	}
	return "Unknown subcommand."
}

// resolveSelfOrTarget returns the player a balance/history request is about: the
// caller's own linked player, or - only for admins - the named player. On failure
// playerID is 0 and msg says why.
func (h *EconomyCommandHandler) resolveSelfOrTarget(ctx context.Context, i *discordgo.InteractionCreate, guildRowID int64, target string) (playerID int64, msg string) {
	target = strings.TrimSpace(target)
	if target != "" {
		if !isAdminInteraction(i) {
			return 0, "Only admins can look at another player's balance. Leave `player` empty to see your own."
		}
		id, err := h.players.FindByDisplayName(ctx, guildRowID, target)
		if err != nil {
			return 0, "Player not found."
		}
		return id, ""
	}
	if h.links == nil {
		return 0, "Account linking is not configured, so your balance cannot be looked up."
	}
	id, linked := h.links.LinkedPlayerID(ctx, guildRowID, i.Member.User.ID)
	if !linked {
		return 0, "🔗 **ACCOUNT NOT LINKED**\nLink your PlayStation username first (use the Link panel), then try again."
	}
	return id, ""
}

func (h *EconomyCommandHandler) balanceMessage(ctx context.Context, guildRowID, playerID int64) string {
	return economyBalanceMessage(ctx, h.economy, guildRowID, playerID)
}

func (h *EconomyCommandHandler) historyMessage(ctx context.Context, guildRowID, playerID int64) string {
	return economyHistoryMessage(ctx, h.economy, guildRowID, playerID)
}

// economyBalanceMessage / economyHistoryMessage are shared by the command and the
// panel buttons so both render identically.
func economyBalanceMessage(ctx context.Context, svc *economy.Service, guildRowID, playerID int64) string {
	balance, err := svc.Balance(ctx, guildRowID, playerID)
	if err != nil {
		return "Could not load the balance right now."
	}
	return fmt.Sprintf("💰 **CHAMPION POINTS BALANCE**\n\nBalance: **%s pts**", formatAmount(balance))
}

func economyHistoryMessage(ctx context.Context, svc *economy.Service, guildRowID, playerID int64) string {
	page, err := svc.History(ctx, guildRowID, playerID, economyHistoryShown, 0)
	if err != nil {
		return "Could not load the transactions right now."
	}
	if len(page.Items) == 0 {
		return "🧾 **RECENT TRANSACTIONS**\n\n_No transactions yet._"
	}
	var b strings.Builder
	b.WriteString("🧾 **RECENT TRANSACTIONS**\n")
	for _, it := range page.Items {
		sign, icon := "+", "➕"
		if it.Amount < 0 {
			sign, icon = "−", "➖"
		}
		mag := it.Amount
		if mag < 0 {
			mag = -mag
		}
		fmt.Fprintf(&b, "\n%s **%s%s** · %s", icon, sign, formatAmount(mag), it.Label)
		// Check for "no reason" first: sanitizeName maps an empty name to "Unknown".
		if strings.TrimSpace(it.Description) != "" {
			fmt.Fprintf(&b, " — %s", safeTrunc(sanitizeName(it.Description), 80))
		}
		fmt.Fprintf(&b, " · balance %s · <t:%d:R>", formatAmount(it.BalanceAfter), it.CreatedAt.Unix())
	}
	return b.String()
}

const economyHistoryShown = 10

func (h *EconomyCommandHandler) adjust(ctx context.Context, i *discordgo.InteractionCreate, guildRowID int64, sub *discordgo.ApplicationCommandInteractionDataOption, credit bool) string {
	name := strings.TrimSpace(optionString(sub, "player"))
	playerID, err := h.players.FindByDisplayName(ctx, guildRowID, name)
	if err != nil {
		return "Player not found."
	}
	req := economy.Request{GuildID: guildRowID, PlayerID: playerID, Amount: optionInt(sub, "amount"), Description: optionString(sub, "reason"), Actor: i.Member.User.ID}
	var res economy.Result
	if credit {
		res, err = h.economy.AdminCredit(ctx, req)
	} else {
		res, err = h.economy.AdminDebit(ctx, req)
	}
	switch {
	case err == nil:
		verb := "Credited"
		if !credit {
			verb = "Debited"
		}
		return fmt.Sprintf("%s **%s pts** %s %s. New balance: **%s pts**.", verb, formatAmount(req.Amount), map[bool]string{true: "to", false: "from"}[credit], sanitizeName(name), formatAmount(res.Balance))
	case errors.Is(err, economy.ErrInsufficientFunds):
		bal, _ := h.economy.Balance(ctx, guildRowID, playerID)
		return fmt.Sprintf("Insufficient balance: %s has **%s pts**, cannot remove %s.", sanitizeName(name), formatAmount(bal), formatAmount(req.Amount))
	case errors.Is(err, economy.ErrInvalidAmount):
		return "Use a positive amount."
	default:
		return "Could not apply the adjustment."
	}
}
