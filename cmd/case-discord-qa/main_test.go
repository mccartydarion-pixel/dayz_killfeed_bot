package main

import (
 "os"
 "strings"
 "testing"

 "github.com/bwmarrin/discordgo"
)

func baseProbeConfig() probeConfig {
 return probeConfig{
  mode:modePreflight,
  guildID:"123456789012345678",
  channelID:"223456789012345678",
  botID:"323456789012345678",
  productionGuildID:"423456789012345678",
  productionBotID:"523456789012345678",
 }
}

func TestQADiscordPreflightNeverRequiresSendGate(t *testing.T){
 t.Setenv("CASE_DISCORD_QA_ALLOW_SEND","")
 c:=baseProbeConfig()
 if err:=c.validate();err!=nil{t.Fatalf("read-only check blocked: %v",err)}
 if err:=run(probeConfig{});err==nil || !strings.Contains(err.Error(),"mode"){
  t.Fatalf("invalid config should fail before token or network: %v",err)
 }
}

func TestQADiscordSendNeedsIndependentConsentAndProductionDenylist(t *testing.T){
 t.Setenv("CASE_DISCORD_QA_ALLOW_SEND","")
 c:=baseProbeConfig()
 c.mode=modeSend
 if err:=c.validate();err==nil{t.Fatal("default send should be denied")}
 c.confirm=sendConfirmation
 if err:=c.validate();err==nil{t.Fatal("CLI approval must not bypass environment approval")}
 t.Setenv("CASE_DISCORD_QA_ALLOW_SEND",sendEnvironment)
 if err:=c.validate();err!=nil{t.Fatalf("fully approved QA probe rejected: %v",err)}
 c.guildID=c.productionGuildID
 if err:=c.validate();err==nil{t.Fatal("production guild accepted")}
 c.guildID="123456789012345678"
 c.botID=c.productionBotID
 if err:=c.validate();err==nil{t.Fatal("production bot accepted")}
 c.botID="323456789012345678"
 c.productionBotID=""
 if err:=c.validate();err==nil{t.Fatal("absent production denylist accepted")}
 c.productionBotID="523456789012345678"
 c.channelID="not-a-snowflake"
 if err:=c.validate();err==nil{t.Fatal("invalid channel accepted")}
}

func TestQAReadOnlyVerifyRequiresExactMessageAndReference(t *testing.T){
 c:=baseProbeConfig()
 c.mode=modeVerify
 if err:=c.validate();err==nil{t.Fatal("missing existing message was accepted")}
 c.messageID="623456789012345678"
 c.reference="CHAMPION-CASE-QA-000102030405060708090A0B"
 if err:=c.validate();err!=nil{t.Fatalf("valid existing QA message rejected: %v",err)}
 c.reference="CHAMPION-CASE-WATCH-47"
 if err:=c.validate();err==nil{t.Fatal("real paid delivery ref accepted for synthetic probe")}
 c.reference="CHAMPION-CASE-QA-000102030405060708090A0B"
 c.lostAckSimulation=true
 if err:=c.validate();err==nil{t.Fatal("lost-ACK simulation allowed in read-only verify")}
}

func TestQAMessageRecognitionDoesNotConfuseLiveWatchOrHumanText(t *testing.T){
 reference,err:=newQAReference()
 if err!=nil||!referencePattern.MatchString(reference){t.Fatalf("bad QA reference: %q %v",reference,err)}
 embed:=qaEmbed(reference)
 m:=&discordgo.Message{
  ID:"723456789012345678",ChannelID:"223456789012345678",
  Author:&discordgo.User{ID:"323456789012345678"},
  Embeds:[]*discordgo.MessageEmbed{embed},
 }
 if !matchesQAProbe(m,"323456789012345678","223456789012345678",reference){t.Fatal("matching QA message not recognized")}
 if matchesQAProbe(m,"523456789012345678","223456789012345678",reference){t.Fatal("foreign bot recognized")}
 if matchesQAProbe(m,"323456789012345678","423456789012345678",reference){t.Fatal("foreign channel recognized")}
 if matchesQAProbe(m,"323456789012345678","223456789012345678","CHAMPION-CASE-WATCH-47"){t.Fatal("real paid digest recognized as synthetic QA")}
 fake:=*m
 fake.Author=&discordgo.User{ID:"human"}
 fake.Content=reference
 if matchesQAProbe(&fake,"323456789012345678","223456789012345678",reference){t.Fatal("human text impersonated QA bot")}
 fake=*m;fake.Embeds=nil;fake.Content=reference
 if matchesQAProbe(&fake,"323456789012345678","223456789012345678",reference){t.Fatal("plain text impersonated QA embed")}
 if _,ok:=os.LookupEnv("CASE_DISCORD_QA_BOT_TOKEN");ok{
  t.Log("QA token is present in environment, but this test never reads or uses it")
 }
}

func TestQADiscordExistingChampionsGuildRequiresSeparateBotPrivateChannelAndDoubleApproval(t *testing.T) {
 t.Setenv("CASE_DISCORD_QA_ALLOW_SEND",sendEnvironment)
 t.Setenv("CASE_DISCORD_QA_EXISTING_GUILD","")
 c:=baseProbeConfig()
 c.mode=modeSend
 c.guildID=c.productionGuildID
 c.useExistingGuild=true
 c.confirm=sendConfirmation
 c.productionChannels="623456789012345678,723456789012345678"
 if err:=c.validate();err==nil{t.Fatal("existing guild without extra consent accepted")}
 c.existingGuildConfirm=existingGuildConfirmation
 if err:=c.validate();err==nil{t.Fatal("CLI-only existing guild approval accepted")}
 t.Setenv("CASE_DISCORD_QA_EXISTING_GUILD",existingGuildEnvironment)
 if err:=c.validate();err!=nil{t.Fatalf("separate bot, private-channel configuration rejected: %v",err)}
 c.channelID="623456789012345678"
 if err:=c.validate();err==nil{t.Fatal("production channel accepted as QA target")}
 c.channelID="223456789012345678"
 c.productionChannels=""
 if err:=c.validate();err==nil{t.Fatal("missing production channel denylist accepted")}
 c.productionChannels="623456789012345678,invalid"
 if err:=c.validate();err==nil{t.Fatal("invalid production channel denylist accepted")}
 c.productionChannels="623456789012345678"
 c.botID=c.productionBotID
 if err:=c.validate();err==nil{t.Fatal("production bot accepted inside existing guild")}
 c.botID="323456789012345678"
 c.guildID="123456789012345678"
 if err:=c.validate();err==nil{t.Fatal("existing guild mode accepted wrong guild")}
}

func TestQADiscordExistingGuildReadOnlyPreflightNeedsNoSendConsent(t *testing.T) {
 t.Setenv("CASE_DISCORD_QA_ALLOW_SEND","")
 t.Setenv("CASE_DISCORD_QA_EXISTING_GUILD","")
 c:=baseProbeConfig()
 c.useExistingGuild=true
 c.guildID=c.productionGuildID
 if err:=c.validate();err!=nil{t.Fatalf("read-only existing guild check blocked: %v",err)}
}
