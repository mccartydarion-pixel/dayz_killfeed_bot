package discord

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BaseRequestDecisionMessage tells a player the server owner approved or
// declined their base registration request. Text is neutralised and mentions
// are disabled.
func BaseRequestDecisionMessage(approved bool, baseName, serverName, reason string) *discordgo.MessageSend {
	name := caseFallback(caseSafeText(baseName, 64), "your base")
	server := caseFallback(caseSafeText(serverName, 100), "your server")
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Base Registration")}
	if approved {
		embed.Title = "✅ " + name + " is registered"
		embed.Color = presentation.SuccessGreen
		embed.Description = "The owner of " + server + " approved your base. You can now use the base services in the Security Store, like the Raid Alarm and Perimeter Watch."
	} else {
		embed.Title = "❌ Your base request wasn't approved"
		embed.Color = presentation.ErrorRed
		embed.Description = "The owner of " + server + " declined your request for " + name + "."
		if r := caseSafeText(reason, 300); r != "" {
			embed.Fields = []*discordgo.MessageEmbedField{{Name: "Reason", Value: r}}
		}
		embed.Footer = presentation.Footer("", "You can send a new request from the Security Store")
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// NewBaseRequestMessage tells the server owner a player asked for a base to
// be registered. Nothing is registered until they approve.
func NewBaseRequestMessage(playerName, baseName, serverName string, radius float64, reviewURL string) *discordgo.MessageSend {
	player := caseFallback(caseSafeText(playerName, 64), "A player")
	embed := &discordgo.MessageEmbed{
		Author:      presentation.BrandAuthor("Base Registration"),
		Title:       "📍 New base request on " + caseFallback(caseSafeText(serverName, 100), "your server"),
		Color:       presentation.InfoSteel,
		Description: "**" + player + "** wants **" + caseFallback(caseSafeText(baseName, 64), "a base") + "** registered. Nothing is registered until you approve it.",
		Fields:      []*discordgo.MessageEmbedField{{Name: "Size", Value: fmt.Sprintf("%.0f m around where they stood", radius), Inline: true}},
	}
	if reviewURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Review", Value: reviewURL, Inline: false})
	} else {
		embed.Footer = presentation.Footer("", "Approve or decline it on the anti-cheat Bases tab")
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// SecurityGiftMessage tells a player the server owner gave them paid time.
func SecurityGiftMessage(serviceLabel, serverName string, days int, endsAt time.Time, note, storeURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author:      presentation.BrandAuthor("Security Store"),
		Title:       fmt.Sprintf("🎁 You've been given %d days of %s", days, caseSafeText(serviceLabel, 40)),
		Color:       presentation.SuccessGreen,
		Description: "A gift from the owner of " + caseFallback(caseSafeText(serverName, 100), "your server") + ". Nothing was charged.",
		Fields:      []*discordgo.MessageEmbedField{{Name: "Active until", Value: presentation.Timestamp(endsAt, 'f'), Inline: true}},
	}
	if n := caseSafeText(note, 200); n != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Note", Value: n})
	}
	if storeURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Security Store", Value: storeURL})
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// BaseRentNoticeMessage reminds a player that rent for their base is due soon
// (dueSoon) or that the base is paused because rent is overdue.
func BaseRentNoticeMessage(dueSoon bool, baseName, serverName string, dueAt time.Time, price int64, days int, storeURL string) *discordgo.MessageSend {
	name := caseFallback(caseSafeText(baseName, 64), "your base")
	server := caseFallback(caseSafeText(serverName, 100), "your server")
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Base Rent")}
	if dueSoon {
		embed.Title = "🏠 Rent for " + name + " is due " + presentation.Timestamp(dueAt, 'R')
		embed.Color = presentation.WarningAmber
		embed.Description = fmt.Sprintf("Pay %s Champion Points for %s more days on %s to keep its base services running. Nothing is taken automatically.",
			presentation.FormatThousands(price), presentation.FormatThousands(int64(days)), server)
	} else {
		embed.Title = "⏸️ " + name + " is paused"
		embed.Color = presentation.ErrorRed
		embed.Description = "Rent on " + server + " is overdue, so this base's alerts, Perimeter Watch, Black Box and faction sharing have stopped. Pay the rent to turn them back on; your base is kept."
	}
	if storeURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Pay in the Security Store", Value: storeURL})
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// WithRentNotice adds the server's rent terms to an approval DM.
func WithRentNotice(msg *discordgo.MessageSend, price int64, days, graceDays int) *discordgo.MessageSend {
	if msg == nil || len(msg.Embeds) == 0 || price <= 0 || days <= 0 {
		return msg
	}
	msg.Embeds[0].Fields = append(msg.Embeds[0].Fields, &discordgo.MessageEmbedField{Name: "Rent",
		Value: fmt.Sprintf("This server charges %s Champion Points every %d days for player bases. Pay it in the Security Store within %d days to keep your base services running.",
			presentation.FormatThousands(price), days, graceDays)})
	return msg
}

// BaseRentGiftMessage tells a base owner the server owner gave them free rent days.
func BaseRentGiftMessage(baseName, serverName string, days int, paidUntil time.Time, note, storeURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author:      presentation.BrandAuthor("Base Rent"),
		Title:       fmt.Sprintf("🎁 %d days of free rent for %s", days, caseFallback(caseSafeText(baseName, 64), "your base")),
		Color:       presentation.SuccessGreen,
		Description: "A gift from the owner of " + caseFallback(caseSafeText(serverName, 100), "your server") + ". Nothing was charged.",
		Fields:      []*discordgo.MessageEmbedField{{Name: "Rent paid until", Value: fmt.Sprintf("<t:%d:f>", paidUntil.Unix()), Inline: true}},
	}
	if n := caseSafeText(note, 200); n != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Note", Value: n})
	}
	if storeURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Security Store", Value: storeURL})
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// BaseRentPaidForYouMessage tells a base owner a faction mate paid their rent.
func BaseRentPaidForYouMessage(payerName, baseName, serverName string, days int, paidUntil time.Time) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author: presentation.BrandAuthor("Base Rent"),
		Title:  "🏠 " + caseFallback(caseSafeText(payerName, 64), "A faction mate") + " paid rent for " + caseFallback(caseSafeText(baseName, 64), "your base"),
		Color:  presentation.SuccessGreen,
		Description: fmt.Sprintf("%d more days on %s, paid from their Champion Points. Nothing was taken from you.",
			days, caseFallback(caseSafeText(serverName, 100), "your server")),
		Fields: []*discordgo.MessageEmbedField{{Name: "Rent paid until", Value: fmt.Sprintf("<t:%d:f>", paidUntil.Unix()), Inline: true}},
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// NewBaseTransferMessage tells the server owner a player asked to hand a base
// to a faction mate. Nothing changes until they approve.
func NewBaseTransferMessage(fromName, toName, baseName, serverName, reviewURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author: presentation.BrandAuthor("Base Registration"),
		Title:  "🔁 Base transfer request on " + caseFallback(caseSafeText(serverName, 100), "your server"),
		Color:  presentation.InfoSteel,
		Description: "**" + caseFallback(caseSafeText(fromName, 64), "A player") + "** wants to hand **" + caseFallback(caseSafeText(baseName, 64), "a base") +
			"** to their faction mate **" + caseFallback(caseSafeText(toName, 64), "a player") + "**. Nothing changes until you approve it.",
	}
	if reviewURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Review", Value: reviewURL})
	} else {
		embed.Footer = presentation.Footer("", "Approve or decline it on the anti-cheat Bases tab")
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// BaseTransferDecisionMessage tells a player the answer to a base transfer.
// receiving is true for the faction mate the base would go to.
func BaseTransferDecisionMessage(approved, receiving bool, baseName, fromName, toName, serverName, reason string) *discordgo.MessageSend {
	name := caseFallback(caseSafeText(baseName, 64), "the base")
	server := caseFallback(caseSafeText(serverName, 100), "your server")
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Base Registration")}
	switch {
	case approved && receiving:
		embed.Title = "🔁 " + name + " is now yours"
		embed.Color = presentation.SuccessGreen
		embed.Description = caseFallback(caseSafeText(fromName, 64), "Your faction mate") + " handed it to you on " + server +
			". Any rent already paid stays with the base. Base services you buy are your own."
	case approved:
		embed.Title = "🔁 " + name + " was handed over"
		embed.Color = presentation.SuccessGreen
		embed.Description = "The owner of " + server + " approved it. The base now belongs to " + caseFallback(caseSafeText(toName, 64), "your faction mate") + "."
	default:
		embed.Title = "❌ The transfer of " + name + " wasn't approved"
		embed.Color = presentation.ErrorRed
		embed.Description = "The owner of " + server + " declined it. Nothing changed."
		if r := caseSafeText(reason, 300); r != "" {
			embed.Fields = []*discordgo.MessageEmbedField{{Name: "Reason", Value: r}}
		}
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}
