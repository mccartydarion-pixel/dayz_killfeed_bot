package discord

import (
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

 "github.com/bwmarrin/discordgo"
)

// Exercises discordgo's REAL REST send/get methods against a local HTTP
// server. No bot token, channel, guild or network call reaches Discord.
func TestCaseWatchDiscordRESTSendAndReadBackExactReference(t *testing.T){
 client,err:=New("fake-only")
 if err!=nil{t.Fatal(err)}
 client.session.State.User=&discordgo.User{ID:"bot"}
 const messageID="123456789012345678"
 const channelID="staff-channel"
 var stored *discordgo.Message
 var posts,gets int
 srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  if r.URL.Path!="/channels/"+channelID+"/messages" &&
    r.URL.Path!="/channels/"+channelID+"/messages/"+messageID{
   t.Errorf("unexpected fake Discord path %s",r.URL.Path)
   w.WriteHeader(http.StatusNotFound);return
  }
  w.Header().Set("Content-Type","application/json")
  switch r.Method {
  case http.MethodPost:
   posts++
   var req struct{Embeds []*discordgo.MessageEmbed `json:"embeds"`}
   if err:=json.NewDecoder(r.Body).Decode(&req);err!=nil{t.Errorf("decode Discord post: %v",err);w.WriteHeader(400);return}
   if len(req.Embeds)!=1{t.Errorf("expected one embed, got %d",len(req.Embeds));w.WriteHeader(400);return}
   stored=&discordgo.Message{ID:messageID,ChannelID:channelID,
    Author:&discordgo.User{ID:"bot"},Embeds:req.Embeds}
   if err:=json.NewEncoder(w).Encode(stored);err!=nil{t.Error(err)}
  case http.MethodGet:
   gets++
   if stored==nil{w.WriteHeader(404);return}
   if err:=json.NewEncoder(w).Encode(stored);err!=nil{t.Error(err)}
  default:t.Errorf("unexpected Discord method %s",r.Method);w.WriteHeader(405)
  }
 }))
 defer srv.Close()
 previous:=discordgo.EndpointChannels
 discordgo.EndpointChannels=srv.URL+"/channels/"
 defer func(){discordgo.EndpointChannels=previous}()
 id:=int64(78)
 embed:=BuildCaseWatchDigestEmbed(AdminAlert{
  GuildRowID:7,ServerID:30,Kind:AlertKindCaseWatchDigest,
  Fields:[][2]string{{"Persisted source lines","5"}}},"QA server")
 embed.Fields=append(embed.Fields,&discordgo.MessageEmbedField{
  Name:"Delivery reference",Value:CaseWatchDeliveryReference(id),
 })
 sent,err:=client.session.ChannelMessageSendComplex(channelID,
  &discordgo.MessageSend{Embeds:[]*discordgo.MessageEmbed{embed}})
 if err!=nil||sent==nil||sent.ID!=messageID{t.Fatalf("fake Discord send failed: %+v %v",sent,err)}
 read,err:=client.session.ChannelMessage(channelID,messageID)
 if err!=nil{t.Fatal(err)}
 if !MatchCaseWatchDeliveredMessage(read,client.BotID(),channelID,id){
  t.Fatalf("Discord REST roundtrip lost digest evidence: %+v",read)
 }
 if MatchCaseWatchDeliveredMessage(read,client.BotID(),channelID,id+1) {
  t.Fatal("a different delivery ID matched the same actual message")
 }
 if posts!=1||gets!=1{t.Fatalf("unexpected REST calls: POST %d GET %d",posts,gets)}
 if !strings.Contains(read.Embeds[0].Description,"source observations"){
  t.Fatal("observational disclaimer missing")
 }
}
