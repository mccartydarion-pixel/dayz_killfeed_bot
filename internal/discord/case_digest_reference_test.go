package discord

import (
 "testing"
 "github.com/bwmarrin/discordgo"
)

func TestCaseWatchReferenceCannotBeForgedByHumanOrOtherMessage(t *testing.T){
 id:=int64(47)
 ref:=CaseWatchDeliveryReference(id)
 message:=&discordgo.Message{
  ID:"99",ChannelID:"staff",Author:&discordgo.User{ID:"bot"},
  Embeds:[]*discordgo.MessageEmbed{{
   Title:"C.A.S.E. WATCH • OBSERVATION DIGEST",
   Description:"Persisted ADM source observations from the selected server.",
   Fields:[]*discordgo.MessageEmbedField{{Name:"Delivery reference",Value:ref}},
  }},
 }
 if !MatchCaseWatchDeliveredMessage(message,"bot","staff",id){t.Fatal("valid bot digest rejected")}
 cases:=[]struct{name string;mutate func(*discordgo.Message)}{
  {"wrong author",func(m *discordgo.Message){m.Author.ID="human"}},
  {"wrong channel",func(m *discordgo.Message){m.ChannelID="public"}},
  {"wrong id",func(m *discordgo.Message){m.Embeds[0].Fields[0].Value=CaseWatchDeliveryReference(48)}},
  {"wrong title",func(m *discordgo.Message){m.Embeds[0].Title="PUBLIC ANNOUNCEMENT"}},
  {"missing provenance",func(m *discordgo.Message){m.Embeds[0].Description="not observations"}},
  {"plain content impersonation",func(m *discordgo.Message){m.Embeds=nil;m.Content=ref}},
 }
 for _,tc:=range cases {t.Run(tc.name,func(t *testing.T){
  copyMessage:=*message
  author:=*message.Author;copyMessage.Author=&author
  embed:=*message.Embeds[0]
  f:=*message.Embeds[0].Fields[0];embed.Fields=[]*discordgo.MessageEmbedField{&f}
  copyMessage.Embeds=[]*discordgo.MessageEmbed{&embed}
  tc.mutate(&copyMessage)
  if MatchCaseWatchDeliveredMessage(&copyMessage,"bot","staff",id){t.Fatal("forged Discord message reconciled")}
 })}
}
