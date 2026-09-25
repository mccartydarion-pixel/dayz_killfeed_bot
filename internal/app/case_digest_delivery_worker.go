package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// caseWatchPrivateDestination always makes a fresh Discord API check in the
// real app, not just a lookup of a route or a channel name. Test overrides
// can simulate privacy failure without contacting Discord.
func (a *App) caseWatchPrivateDestination(ctx context.Context,guildSnowflake,channelID string) error {
	if a.caseWatchPrivacyCheck!=nil{return a.caseWatchPrivacyCheck(ctx,guildSnowflake,channelID)}
	if a.Discord==nil{return discord.ErrCaseStaffChannelUnsafe}
	return a.Discord.VerifyCaseStaffChannel(ctx,guildSnowflake,channelID)
}

func (a *App) sendCaseWatchMessage(ctx context.Context,channel string,embed *discordgo.MessageEmbed)(string,error){
	if a.caseWatchSender!=nil{return a.caseWatchSender(ctx,channel,embed)}
	if a.Discord==nil || a.Discord.Session()==nil || ctx.Err()!=nil{return "",errors.New("Discord unavailable")}
	msg,err:=a.Discord.Session().ChannelMessageSendComplex(channel,
		&discordgo.MessageSend{Embeds:[]*discordgo.MessageEmbed{embed}})
	if err!=nil{return "",err}
	if msg==nil || msg.ID==""{return "",errors.New("Discord response missing message identity")}
	return msg.ID,nil
}

// runCaseDigestWorker has no extra Nitrado poll, separate API listener, or
// scheduled automatic digest creation. It only drains staff-initiated rows.
func (a *App) runCaseDigestWorker(ctx context.Context){
	ticker:=time.NewTicker(10*time.Second)
	defer ticker.Stop()
	for{
		if ctx.Err()!=nil{return}
		for i:=0;i<10 && ctx.Err()==nil;i++{
			worked,err:=a.processOneCaseDigest(ctx)
			if err!=nil {
				slog.Warn("component=case","event","watch_outbox_worker_error","err",err.Error())
				break
			}
			if !worked{break}
		}
		select {case <-ctx.Done():return;case <-ticker.C:}
	}
}

// One claim is exclusively owned by its monotonically increasing version.
// Pre-send denial is terminal or retried as appropriate; external network
// uncertainty after BeginSend is UNKNOWN, never blindly resent.
func (a *App) processOneCaseDigest(ctx context.Context)(bool,error){
	store:=a.CaseDigestOutbox
	if store==nil{return false,nil}
	if err:=store.Sweep(ctx);err!=nil{return false,err}
	d,err:=store.ClaimNext(ctx)
	if err!=nil || d==nil{return false,err}
	// All transitions use a fresh bounded context. Once a Discord request has
	// been attempted we must persist its result even during service shutdown.
	finish:=func(operation func(context.Context)error) error {
		saveCtx,cancel:=context.WithTimeout(context.Background(),8*time.Second)
		defer cancel()
		return operation(saveCtx)
	}
	block:=func(reason string)(bool,error){
		return true,finish(func(c context.Context)error{return store.BlockBeforeSend(c,*d,reason)})
	}
	retry:=func(reason string)(bool,error){
		return true,finish(func(c context.Context)error{return store.RetryBeforeSend(c,*d,reason)})
	}
	if a.Billing==nil || a.ClientAdmin==nil || a.SaaSChannelRoutes==nil {
		return retry("DEPENDENCY_UNAVAILABLE")
	}
	scope,err:=a.ClientAdmin.Scope(ctx,d.OrganizationID,d.InstallationID)
	if err!=nil{
		if errors.Is(err,repository.ErrInstallationScopeNotFound){return block("SCOPE_CHANGED")}
		return retry("SCOPE_LOOKUP_FAILED")
	}
	if scope.GuildID!=d.GuildID || scope.ServerID==nil || *scope.ServerID!=d.GameServerID ||
		scope.DiscordGuildID==""{return block("SCOPE_CHANGED")}
	requesterAllowed,requesterErr:=a.caseWatchRequesterAllowed(ctx,scope,d.RequesterUserID)
	if requesterErr!=nil{return retry("REQUESTER_LOOKUP_FAILED")}
	if !requesterAllowed{return block("REQUESTER_ACCESS_REVOKED")}
	allowed,err:=a.caseWorkerAllowed(ctx,d.OrganizationID,d.InstallationID,d.GameServerID,casebilling.CapWatch)
	if err!=nil{return retry("BILLING_LOOKUP_FAILED")}
	if !allowed{return block("PREMIUM_ACCESS_REVOKED")}
	// Resolve against PostgreSQL, NOT a potentially stale runtime route cache.
	channel,found,err:=a.SaaSChannelRoutes.ResolveChannel(ctx,d.GuildID,d.GameServerID,routing.RouteAdminAlerts)
	if err!=nil{return retry("ROUTE_LOOKUP_FAILED")}
	if !found || channel=="" {return block("STAFF_ROUTE_MISSING")}
	if err:=a.caseWatchPrivateDestination(ctx,scope.DiscordGuildID,channel);err!=nil{
		return block("STAFF_CHANNEL_NOT_PRIVATE")
	}
	// Recheck both the original actor and payment after potentially slow
	// Discord privacy lookups, immediately before entering SENDING.
	requesterAllowed,requesterErr=a.caseWatchRequesterAllowed(ctx,scope,d.RequesterUserID)
	if requesterErr!=nil{return retry("REQUESTER_LOOKUP_FAILED")}
	if !requesterAllowed{return block("REQUESTER_ACCESS_REVOKED")}
	allowed,err=a.caseWorkerAllowed(ctx,d.OrganizationID,d.InstallationID,d.GameServerID,casebilling.CapWatch)
	if err!=nil{return retry("BILLING_LOOKUP_FAILED")}
	if !allowed{return block("PREMIUM_ACCESS_REVOKED")}
	// Final atomic DB check of current binding and exact route immediately
	// before irrevocably entering SENDING. A route/scope switch denies send.
	if err:=store.BeginSend(ctx,*d,channel);err!=nil{
		if errors.Is(err,repository.ErrCaseDigestClaimLost){return block("ROUTE_OR_CLAIM_CHANGED")}
		return retry("BEGIN_SEND_FAILED")
	}
	coverage:="Opt-in collector configured; counts are source observations only"
	if !d.CollectorEnabled {coverage="Collector was disabled when requested; historical lines may remain"}
	alert:=discord.AdminAlert{
		GuildRowID:d.GuildID,ServerID:d.GameServerID,Kind:discord.AlertKindCaseWatchDigest,
		At:d.WindowEnd,
		Fields:[][2]string{
			{"Window","Past 24 hours, by ingestion time"},
			{"Persisted source lines",fmt.Sprintf("%d",d.SourceLines)},
			{"Persisted hits",fmt.Sprintf("%d",d.HitLines)},
			{"Persisted kills",fmt.Sprintf("%d",d.KillLines)},
			{"Coverage",coverage},
		},
	}
	name:=""
	if a.serverNameFunc()!=nil {name=a.serverNameFunc()(d.GameServerID)}
	messageID,sendErr:=a.sendCaseWatchMessage(ctx,channel,discord.BuildCaseWatchDigestEmbed(alert,name))
	if sendErr!=nil || messageID==""{
		// A timeout or Discord error is NOT proof no message was sent.
		if saveErr:=finish(func(c context.Context)error{return store.MarkUnknown(c,*d,"DISCORD_ACK_UNCONFIRMED")});saveErr!=nil{
			slog.Error("component=case","event","digest_ambiguous_receipt_save_failed","digest_id",d.ID,"err",saveErr.Error())
			return true,saveErr
		}
		slog.Warn("component=case","event","digest_delivery_unconfirmed","digest_id",d.ID)
		return true,nil
	}
	if err:=finish(func(c context.Context)error{return store.MarkSent(c,*d,messageID)});err!=nil{
		// Discord accepted the message. Never retry; stale SENDING sweeps to
		// UNKNOWN if the receipt write fails. Log for manual reconciliation.
		slog.Error("component=case","event","digest_receipt_save_failed","digest_id",d.ID,"err",err.Error())
		return true,err
	}
	slog.Info("component=case","event","digest_delivered","digest_id",d.ID,"server_id",d.GameServerID)
	return true,nil
}
