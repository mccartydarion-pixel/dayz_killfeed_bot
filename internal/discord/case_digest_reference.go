package discord

import (
 "fmt"
 "strings"

 "github.com/bwmarrin/discordgo"
)

// CaseWatchDeliveryReference is visible on the sent observation embed and
// survives a lost network response. It is not a secret or an access token.
func CaseWatchDeliveryReference(id int64) string {
 return fmt.Sprintf("CHAMPION-CASE-WATCH-%d",id)
}

// MatchCaseWatchDeliveredMessage only recognizes an actual bot-authored
// observation digest with the exact persisted reference in the stored channel.
// A human's text claiming the reference cannot reconcile an UNKNOWN row.
func MatchCaseWatchDeliveredMessage(message *discordgo.Message, botID,channelID string,deliveryID int64) bool {
 if message==nil || message.ID=="" || message.ChannelID!=channelID ||
  botID=="" || message.Author==nil || message.Author.ID!=botID ||
  deliveryID<=0 {return false}
 expected:=CaseWatchDeliveryReference(deliveryID)
 for _,embed:=range message.Embeds {
  if embed==nil || embed.Title!="C.A.S.E. WATCH • OBSERVATION DIGEST" {continue}
  if !strings.Contains(embed.Description,"Persisted ADM source observations") {continue}
  for _,field:=range embed.Fields {
   if field!=nil && field.Name=="Delivery reference" && field.Value==expected {return true}
  }
 }
 return false
}
