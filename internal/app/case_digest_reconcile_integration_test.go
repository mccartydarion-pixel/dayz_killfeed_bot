//go:build integration

package app

import (
 "context"
 "errors"
 "net/http"
 "strconv"
 "strings"
 "sync/atomic"
 "testing"

 "github.com/bwmarrin/discordgo"
 "github.com/yourname/dayz-killfeed/internal/discord"
)

func TestCASEWatchUNKNOWNReconcileNeedsExactBotMessageAndNeverResends(t *testing.T){
 w,store,input:=setupCaseDigestWorld(t)
 id,err:=store.Enqueue(context.Background(),input)
 if err!=nil{t.Fatal(err)}
 var sends atomic.Int64
 var actualEmbed *discordgo.MessageEmbed
 w.a.caseWatchSender=func(_ context.Context,channel string,embed *discordgo.MessageEmbed)(string,error){
  sends.Add(1)
  actualEmbed=embed
  if channel!="private-staff-channel"{t.Fatalf("incorrect channel %s",channel)}
  return "",errors.New("timeout after Discord may have accepted")
 }
 worked,err:=w.a.processOneCaseDigest(context.Background())
 if err!=nil||!worked||sends.Load()!=1{t.Fatalf("initial attempt: %v %v %d",worked,err,sends.Load())}
 receipt,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
 if err!=nil||receipt==nil||receipt.Status!="UNKNOWN"||receipt.DiscordChannelID==nil{
  t.Fatalf("unknown delivery was not persisted: %+v %v",receipt,err)
 }
 reference:=discord.CaseWatchDeliveryReference(id)
 foundReference:=false
 for _,field:=range actualEmbed.Fields{
  if field.Name=="Delivery reference"&&field.Value==reference{foundReference=true}
 }
 if !foundReference{t.Fatal("attempted Discord message lacks exact reconciliation marker")}
 path:=w.path("/anti-cheat/premium/watch-digest/"+strconv.FormatInt(id,10)+"/reconcile")
 values:=map[string]string{"deliveryID":strconv.FormatInt(id,10)}
 messageID:="123456789012345678"
 msg:=&discordgo.Message{
  ID:messageID,ChannelID:"private-staff-channel",Author:&discordgo.User{ID:"bot"},
  Embeds:[]*discordgo.MessageEmbed{actualEmbed},
 }
 w.a.caseWatchMessageLookup=func(_ context.Context,channel,id string)(*discordgo.Message,string,error){
  if channel!="private-staff-channel"||id!=messageID{t.Fatalf("lookup escaped original channel: %s/%s",channel,id)}
  return msg,"bot",nil
 }
 for _,bad:=range []struct{name string;modify func()}{
  {"human message",func(){msg.Author.ID="human"}},
  {"wrong guild channel",func(){msg.ChannelID="public"}},
  {"different digest",func(){msg.Embeds[0].Fields[len(msg.Embeds[0].Fields)-1].Value=discord.CaseWatchDeliveryReference(id+1)}},
 }{
  t.Run(bad.name,func(t *testing.T){
   copyAuthor:=*msg.Author
   copyEmbed:=*actualEmbed
   fields:=make([]*discordgo.MessageEmbedField,len(actualEmbed.Fields))
   for i,f:=range actualEmbed.Fields {x:=*f;fields[i]=&x}
   copyEmbed.Fields=fields
   msg.Author=&copyAuthor;msg.ChannelID="private-staff-channel";msg.Embeds=[]*discordgo.MessageEmbed{&copyEmbed}
   bad.modify()
   rr:=w.call(w.a.handleAntiCheatWatchDigestReconcile,http.MethodPost,path,w.f.OwnerDiscordID,
    map[string]string{"messageId":messageID},values)
   if rr.Code!=http.StatusBadRequest{t.Fatalf("forged message accepted: %d %s",rr.Code,rr.Body.String())}
   receipt,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
   if err!=nil||receipt.Status!="UNKNOWN"{t.Fatalf("forged message changed receipt: %+v %v",receipt,err)}
  })
 }
 msg.Author=&discordgo.User{ID:"bot"}
 msg.ChannelID="private-staff-channel"
 msg.Embeds=[]*discordgo.MessageEmbed{actualEmbed}
 // Reconciliation is restricted to owner; members cannot repair receipts.
 foreign:=syncUser(t,w.a,"case-reconcile-foreign-"+strconv.FormatInt(id,10),"Stranger")
 rr:=w.call(w.a.handleAntiCheatWatchDigestReconcile,http.MethodPost,path,foreign.DiscordUserID,
  map[string]string{"messageId":messageID},values)
 if rr.Code!=http.StatusForbidden{t.Fatalf("foreign actor accepted: %d",rr.Code)}
 rr=w.call(w.a.handleAntiCheatWatchDigestReconcile,http.MethodPost,path,w.f.OwnerDiscordID,
  map[string]string{"messageId":messageID},values)
 if rr.Code!=http.StatusOK{t.Fatalf("real bot message rejected: %d %s",rr.Code,rr.Body.String())}
 out:=decodeBody[map[string]any](t,rr)
 if out["status"]!="SENT"||out["verification"]!="BOT_AUTHORED_MATCHING_DISCORD_MESSAGE"{
  t.Fatalf("invalid repair claim %v",out)
 }
 receipt,err=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
 if err!=nil||receipt.Status!="SENT"||receipt.DiscordMessageID==nil||*receipt.DiscordMessageID!=messageID{
  t.Fatalf("receipt not confirmed: %+v %v",receipt,err)
 }
 if receipt.ReasonCode==nil||!strings.Contains(*receipt.ReasonCode,"MANUALLY_VERIFIED"){
  t.Fatalf("missing reconciliation audit reason: %+v",receipt)
 }
 rr=w.call(w.a.handleAntiCheatWatchDigestReconcile,http.MethodPost,path,w.f.OwnerDiscordID,
  map[string]string{"messageId":messageID},values)
 if rr.Code!=http.StatusConflict{t.Fatalf("second verification overwrote terminal receipt: %d",rr.Code)}
 for i:=0;i<2;i++{
  worked,err=w.a.processOneCaseDigest(context.Background())
  if err!=nil||worked||sends.Load()!=1{t.Fatalf("reconciliation caused resend: %v %v calls %d",worked,err,sends.Load())}
 }
}

func TestCASEWatchReceiptCannotBeReconciledOutsideSelectedServer(t *testing.T){
 w,store,input:=setupCaseDigestWorld(t)
 id,err:=store.Enqueue(context.Background(),input)
 if err!=nil{t.Fatal(err)}
 d,err:=store.ClaimNext(context.Background())
 if err!=nil||d==nil{t.Fatal(err)}
 if err:=store.BeginSend(context.Background(),*d,"private-staff-channel");err!=nil{t.Fatal(err)}
 if err:=store.MarkUnknown(context.Background(),*d,"DISCORD_ACK_UNCONFIRMED");err!=nil{t.Fatal(err)}
 // A wrong tenant/server cannot write a receipt even if it knows a message ID.
 ok,err:=store.ReconcileUnknownWithVerifiedDiscordMessage(context.Background(),
  w.f.OrgID,w.f.InstallationID,w.serverID+1,id,"private-staff-channel","123456789012345678")
 if err!=nil||ok{t.Fatalf("other server reconciled receipt: %v %v",ok,err)}
 row,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
 if err!=nil||row.Status!="UNKNOWN"{t.Fatalf("wrong-server change: %+v %v",row,err)}
}
