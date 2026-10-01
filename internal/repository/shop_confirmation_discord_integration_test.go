//go:build integration

package repository

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Migration 0088 against a real PostgreSQL: the delivered-order notice lease, the Discord buyer
// lookup, the ticket channel lease and the per-server ticket setup. Nothing contacts Discord.

// link gives installation A's buyer a Discord account with the given link status and returns its id.
func (w *confirmationWorld) link(status string) string {
	w.t.Helper()
	discordID := fmt.Sprintf("d%d", time.Now().UnixNano()%1_000_000_000_000)
	_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status, verified_at)
VALUES($1,$2,$3,$4,NOW())`, w.a.GuildRowID, w.playerA, discordID, status)
	must(w.t, err)
	return discordID
}

// setLinkStatus changes the status of that link, keeping the same Discord account.
func (w *confirmationWorld) setLinkStatus(status string) {
	w.t.Helper()
	_, err := w.db.Pool.Exec(w.ctx, `UPDATE player_links SET status=$3 WHERE guild_id=$1 AND player_id=$2`, w.a.GuildRowID, w.playerA, status)
	must(w.t, err)
}

// claim leases pending notices and returns the one of the given order (nil when it was not claimed).
func (w *confirmationWorld) claim(o *order, now time.Time) *ShopOrderNotice {
	w.t.Helper()
	for {
		notices, err := w.conf.ClaimPendingNotices(w.ctx, now, 100)
		must(w.t, err)
		for i := range notices {
			if notices[i].PurchaseID == o.purchase {
				return &notices[i]
			}
		}
		if len(notices) < 100 {
			return nil
		}
	}
}

func (w *confirmationWorld) notifyState(o *order) (state string, attempts int, channel, message *string) {
	w.t.Helper()
	must(w.t, w.db.Pool.QueryRow(w.ctx, `SELECT notify_state, notify_attempts, dm_channel_id, dm_message_id FROM shop_order_confirmations WHERE purchase_id=$1`, o.purchase).
		Scan(&state, &attempts, &channel, &message))
	return
}

func TestNoticeLeaseAndOutcome(t *testing.T) {
	w := newConfirmationWorld(t)
	discordID := w.link("VERIFIED")
	o := w.delivered()
	now := time.Now()

	n := w.claim(o, now)
	if n == nil {
		t.Fatal("a delivered order was not offered for its notice")
	}
	if n.BuyerDiscordID != discordID || n.OrganizationID != o.f.OrgID || n.InstallationID != o.f.InstallationID || n.GuildRowID != o.f.GuildRowID ||
		!strings.HasPrefix(n.DiscordGuildID, "saas-fixture-guild-") || n.GuildName != "Fixture Guild" || n.TotalPoints != 1 || n.Attempts != 1 ||
		len(n.Items) != 1 || n.Items[0].Name != "Canary BandageDressing" || n.Items[0].Quantity != 1 {
		t.Fatalf("notice = %+v", *n)
	}
	// While the lease is held nobody else gets it; after the lease it is offered again.
	if again := w.claim(o, now.Add(time.Minute)); again != nil {
		t.Fatal("a leased notice was claimed a second time")
	}
	retry := w.claim(o, now.Add(shopDiscordLease+time.Second))
	if retry == nil || retry.Attempts != 2 {
		t.Fatalf("after the lease: %+v, want a second attempt", retry)
	}

	must(t, w.conf.MarkNotice(w.ctx, o.purchase, NoticeSent, "dm-channel", "dm-message"))
	state, attempts, channel, message := w.notifyState(o)
	if state != NoticeSent || attempts != 2 || channel == nil || *channel != "dm-channel" || message == nil || *message != "dm-message" {
		t.Fatalf("after SENT: state=%s attempts=%d channel=%v message=%v", state, attempts, channel, message)
	}
	// A late result never overwrites the first one, and a sent notice is never offered again.
	must(t, w.conf.MarkNotice(w.ctx, o.purchase, NoticeUnavailable, "", ""))
	if state, _, _, _ := w.notifyState(o); state != NoticeSent {
		t.Fatalf("a late UNAVAILABLE overwrote SENT: %s", state)
	}
	if again := w.claim(o, now.Add(time.Hour)); again != nil {
		t.Fatal("a sent notice was claimed again")
	}
	if err := w.conf.MarkNotice(w.ctx, o.purchase, "PENDING", "", ""); err == nil {
		t.Fatal("a notice was marked PENDING")
	}
}

func TestNoticeRetriesRunOutAndAnsweredOrdersAreSkipped(t *testing.T) {
	w := newConfirmationWorld(t)
	w.link("VERIFIED")

	o := w.delivered()
	at := time.Now()
	for attempt := 1; attempt <= ShopNoticeMaxAttempts; attempt++ {
		n := w.claim(o, at)
		if n == nil || n.Attempts != attempt {
			t.Fatalf("attempt %d: %+v", attempt, n)
		}
		must(t, w.conf.ReleaseNotice(w.ctx, o.purchase))
		wantState := NoticePending
		if attempt == ShopNoticeMaxAttempts {
			wantState = NoticeUnavailable
		}
		if state, _, _, _ := w.notifyState(o); state != wantState {
			t.Fatalf("after release %d: state=%s, want %s", attempt, state, wantState)
		}
		at = at.Add(shopDiscordLease + time.Second)
	}
	if n := w.claim(o, at); n != nil {
		t.Fatal("a notice that ran out of attempts was claimed again")
	}

	// An order answered on the website before the DM went out needs no DM.
	answered := w.delivered()
	_, err := w.conf.ConfirmReceived(w.ctx, answered.f.OrgID, answered.f.InstallationID, answered.purchase, w.playerA, ConfirmationViaSite)
	must(t, err)
	if n := w.claim(answered, time.Now()); n != nil {
		t.Fatal("an already answered order was offered for a notice")
	}
}

func TestNoticeWithoutAVerifiedAccountHasNoRecipient(t *testing.T) {
	w := newConfirmationWorld(t)
	w.link("PENDING") // a link that is not verified proves nothing
	o := w.delivered()
	n := w.claim(o, time.Now())
	if n == nil || n.BuyerDiscordID != "" {
		t.Fatalf("notice = %+v, want one without a recipient", n)
	}
}

func TestOrderForDiscordBuyerNeedsTheVerifiedLink(t *testing.T) {
	w := newConfirmationWorld(t)
	discordID := w.link("VERIFIED")
	o := w.delivered()

	got, err := w.conf.OrderForDiscordBuyer(w.ctx, o.purchase, discordID)
	must(t, err)
	c := got.Confirmation
	if c.PurchaseID != o.purchase || c.OrganizationID != o.f.OrgID || c.InstallationID != o.f.InstallationID || c.PlayerID != w.playerA ||
		c.State != ConfirmationAwaitingBuyer || got.GuildRowID != o.f.GuildRowID || got.ServerID != o.f.ServerRowID || got.DiscordGuildID == "" || got.Status == "" {
		t.Fatalf("order = %+v", got)
	}

	_, err = w.conf.OrderForDiscordBuyer(w.ctx, o.purchase, "someone-else")
	wantIs(t, "another Discord account", err, ErrShopConfirmationNotFound)
	_, err = w.conf.OrderForDiscordBuyer(w.ctx, o.purchase+1_000_000, discordID)
	wantIs(t, "an unknown purchase", err, ErrShopConfirmationNotFound)

	// Once the link is no longer verified the same account reaches nothing.
	w.setLinkStatus("REJECTED")
	_, err = w.conf.OrderForDiscordBuyer(w.ctx, o.purchase, discordID)
	wantIs(t, "an unverified link", err, ErrShopConfirmationNotFound)
}

func (w *confirmationWorld) claimTicket(ticketID int64, now time.Time, only int64) *ShopTicketChannelJob {
	w.t.Helper()
	for {
		jobs, err := w.conf.ClaimTicketsNeedingChannel(w.ctx, now, only, 50)
		must(w.t, err)
		for i := range jobs {
			if jobs[i].Ticket.ID == ticketID {
				return &jobs[i]
			}
		}
		if len(jobs) < 50 {
			return nil
		}
	}
}

func TestTicketChannelLease(t *testing.T) {
	w := newConfirmationWorld(t)
	o, other := w.delivered(), w.delivered()
	_, ticket, err := w.issue(o, "never arrived")
	must(t, err)
	_, otherTicket, err := w.issue(other, "wrong item")
	must(t, err)
	now := time.Now()

	// The immediate attempt claims only the ticket it was asked for.
	job := w.claimTicket(ticket.ID, now, ticket.ID)
	if job == nil || job.Attempts != 1 || job.GuildRowID != o.f.GuildRowID || job.DiscordGuildID == "" || job.Ticket.Reason != "never arrived" ||
		job.Ticket.OpenedByDiscordID != "buyer-discord" || len(job.Items) != 1 {
		t.Fatalf("job = %+v", job)
	}
	if again := w.claimTicket(ticket.ID, now.Add(time.Minute), ticket.ID); again != nil {
		t.Fatal("a leased ticket was claimed a second time")
	}
	if got := w.claimTicket(otherTicket.ID, now.Add(time.Minute), 0); got == nil {
		t.Fatal("the sweep did not offer the other ticket")
	}

	// A failed attempt is released and retried until the attempts run out.
	for attempt := 2; attempt <= ShopTicketChannelMaxAttempts; attempt++ {
		must(t, w.conf.ReleaseTicketChannel(w.ctx, ticket.ID))
		if job := w.claimTicket(ticket.ID, now, 0); job == nil || job.Attempts != attempt {
			t.Fatalf("attempt %d: %+v", attempt, job)
		}
	}
	must(t, w.conf.ReleaseTicketChannel(w.ctx, ticket.ID))
	if job := w.claimTicket(ticket.ID, now.Add(time.Hour), 0); job != nil {
		t.Fatal("a ticket that ran out of attempts was claimed again")
	}

	// A ticket with a channel, and a resolved ticket, need nothing.
	must(t, w.conf.ReleaseTicketChannel(w.ctx, otherTicket.ID))
	_, err = w.conf.SetTicketChannel(w.ctx, other.f.OrgID, other.f.InstallationID, otherTicket.ID, "123456789012345678")
	must(t, err)
	if job := w.claimTicket(otherTicket.ID, now.Add(time.Hour), 0); job != nil {
		t.Fatal("a ticket that already has a channel was claimed")
	}
	third := w.delivered()
	_, resolved, err := w.issue(third, "sorted out by voice")
	must(t, err)
	_, err = w.conf.ResolveTicket(w.ctx, third.f.OrgID, third.f.InstallationID, resolved.ID, third.f.OwnerUserID, TicketResolutionCompleted, "")
	must(t, err)
	if job := w.claimTicket(resolved.ID, now.Add(time.Hour), 0); job != nil {
		t.Fatal("a resolved ticket was claimed for a channel")
	}
}

func TestTicketDiscordSetupRoundTrip(t *testing.T) {
	w := newConfirmationWorld(t)

	empty, err := w.conf.TicketDiscordSetup(w.ctx, w.a.GuildRowID)
	must(t, err)
	if empty != (ShopTicketDiscordSetup{}) {
		t.Fatalf("a server without a setup = %+v", empty)
	}
	partial := ShopTicketDiscordSetup{OwnerRoleID: "111", StaffRoleID: "222"}
	must(t, w.conf.SaveTicketDiscordSetup(w.ctx, w.a.GuildRowID, partial))
	full := ShopTicketDiscordSetup{OwnerRoleID: "111", StaffRoleID: "333", CategoryID: "444"}
	must(t, w.conf.SaveTicketDiscordSetup(w.ctx, w.a.GuildRowID, full))
	got, err := w.conf.TicketDiscordSetup(w.ctx, w.a.GuildRowID)
	must(t, err)
	if got != full {
		t.Fatalf("setup = %+v, want %+v", got, full)
	}
	// Each Discord server has its own.
	otherGuild, err := w.conf.TicketDiscordSetup(w.ctx, w.b.GuildRowID)
	must(t, err)
	if otherGuild != (ShopTicketDiscordSetup{}) {
		t.Fatalf("another server sees %+v", otherGuild)
	}

}
