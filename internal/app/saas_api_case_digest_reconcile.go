package app

import (
 "context"
 "errors"
 "net/http"
 "strconv"
 "strings"
 "time"

 "github.com/bwmarrin/discordgo"
 "github.com/yourname/dayz-killfeed/internal/discord"
 "github.com/yourname/dayz-killfeed/internal/permissions"
)

// Read-only lookup. Never use message search or a user's pasted screenshot as
// proof; verify the exact Discord channel and message ID under the bot's own
// identity. There is deliberately no resend action.
func (a *App) lookupCaseWatchDiscordMessage(ctx context.Context,channelID,messageID string)(*discordgo.Message,string,error){
 if a.caseWatchMessageLookup!=nil{return a.caseWatchMessageLookup(ctx,channelID,messageID)}
 if a.Discord==nil || a.Discord.Session()==nil || ctx.Err()!=nil{
  return nil,"",errors.New("Discord read unavailable")
 }
 botID:=a.Discord.BotID()
 if botID==""{return nil,"",errors.New("bot identity unavailable")}
 message,err:=a.Discord.Session().ChannelMessage(channelID,messageID)
 return message,botID,err
}

type caseDigestReconcileRequest struct{MessageID string `json:"messageId"`}

// Owner-only receipt repair: verify a real, bot-authored Discord message
// containing the exact immutable digest reference in the previously attempted
// channel, then transition UNKNOWN -> SENT through a scoped DB CAS.
// This never sends a Discord message or grants C.A.S.E. access.
func (a *App) handleAntiCheatWatchDigestReconcile(w http.ResponseWriter,r *http.Request){
 ac,ok:=a.resolveAdminActor(w,r)
 if !ok{return}
 if ac.level!=permissions.LevelOwner{
  writeSaaSError(w,codeAdminForbidden,"only the organization owner can reconcile a delivery receipt");return
 }
 if ac.scope.ServerID==nil || a.CaseDigestOutbox==nil{
  writeSaaSError(w,codeCaseAccessUnavailable,"delivery receipt unavailable");return
 }
 id,good:=pathInt64(w,r,"deliveryID")
 if !good{return}
 if !enforceRateLimit(w,a.saasAdminActionLimiter,rateLimitKey(r)){return}
 var body caseDigestReconcileRequest
 if !decodeFactionBody(w,r,&body){return}
 messageID:=strings.TrimSpace(body.MessageID)
 n,err:=strconv.ParseUint(messageID,10,64)
 if err!=nil||n==0||len(messageID)>22{
  writeSaaSError(w,codeInvalidRequest,"provide the exact Discord message ID");return
 }
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 row,err:=a.CaseDigestOutbox.GetScoped(ctx,ac.scope.OrganizationID,ac.scope.InstallationID,id)
 if err!=nil{writeSaaSError(w,codeCaseAccessUnavailable,"could not verify delivery receipt");return}
 if row==nil || row.GameServerID!=*ac.scope.ServerID{
  writeSaaSError(w,codeNotFound,"receipt not found for selected server");return
 }
 if row.Status!="UNKNOWN" || row.DiscordChannelID==nil || *row.DiscordChannelID=="" ||
   row.DiscordMessageID!=nil{
  writeSaaSError(w,codeConflict,"only an UNKNOWN delivery with a saved destination can be reconciled");return
 }
 msg,botID,err:=a.lookupCaseWatchDiscordMessage(ctx,*row.DiscordChannelID,messageID)
 if err!=nil{
  writeSaaSError(w,codeCaseAccessUnavailable,"Discord could not confirm that message");return
 }
 if !discord.MatchCaseWatchDeliveredMessage(msg,botID,*row.DiscordChannelID,id) ||
   msg.ID!=messageID{
  writeSaaSError(w,codeInvalidRequest,"Discord message does not match this delivery reference and bot");return
 }
 confirmed,err:=a.CaseDigestOutbox.ReconcileUnknownWithVerifiedDiscordMessage(ctx,
  ac.scope.OrganizationID,ac.scope.InstallationID,*ac.scope.ServerID,id,*row.DiscordChannelID,messageID)
 if err!=nil{writeSaaSError(w,codeCaseAccessUnavailable,"receipt reconciliation failed");return}
 if !confirmed{writeSaaSError(w,codeConflict,"receipt changed during reconciliation");return}
 a.recordAudit(ctx,ac,"CASE_WATCH_DISCORD_RECEIPT_VERIFIED",strconv.FormatInt(id,10),"Discord message independently verified","success",nil,
  map[string]any{"serverId":*ac.scope.ServerID,"deliveryId":id,"channelId":*row.DiscordChannelID,"messageId":messageID})
 writeSaaSJSON(w,http.StatusOK,map[string]any{
  "deliveryId":id,"status":"SENT","messageId":messageID,
  "verification":"BOT_AUTHORED_MATCHING_DISCORD_MESSAGE",
  "note":"The existing Discord message was verified. No new message was sent.",
  "verifiedAt":time.Now().UTC(),
 })
}
