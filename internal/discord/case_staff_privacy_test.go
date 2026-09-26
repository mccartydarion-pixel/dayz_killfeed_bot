package discord

import (
 "errors"
 "testing"

 "github.com/bwmarrin/discordgo"
)

func privateCaseFixture() (*discordgo.Guild,*discordgo.Channel) {
 guild:=&discordgo.Guild{
  ID:"guild",OwnerID:"owner",
  Roles:[]*discordgo.Role{
   {ID:"guild",Permissions:discordgo.PermissionViewChannel},
   {ID:"bot-role",Permissions:discordgo.PermissionViewChannel|
      discordgo.PermissionSendMessages|discordgo.PermissionEmbedLinks|discordgo.PermissionReadMessageHistory},
   {ID:"staff",Permissions:discordgo.PermissionManageGuild},
   {ID:"verified",Permissions:discordgo.PermissionViewChannel},
  },
 }
 channel:=&discordgo.Channel{ID:"staff-channel",GuildID:"guild",Type:discordgo.ChannelTypeGuildText,
  PermissionOverwrites:[]*discordgo.PermissionOverwrite{
   {ID:"guild",Type:discordgo.PermissionOverwriteTypeRole,Deny:discordgo.PermissionViewChannel},
   {ID:"bot",Type:discordgo.PermissionOverwriteTypeMember,
    Allow:discordgo.PermissionViewChannel|discordgo.PermissionSendMessages|discordgo.PermissionEmbedLinks|discordgo.PermissionReadMessageHistory},
  },
 }
 return guild,channel
}

func TestCaseStaffChannelRequiresPrivateEveryoneAndBotPermissions(t *testing.T) {
 g,ch:=privateCaseFixture()
 assert:=func(name string,mutate func(*discordgo.Guild,*discordgo.Channel),want bool) {
  t.Run(name,func(t *testing.T){
   gg,cc:=privateCaseFixture()
   mutate(gg,cc)
   err:=validateCaseStaffChannel(gg,cc,nil,"bot",[]string{"bot-role"})
   if (err==nil)!=want{t.Fatalf("got %v; want valid=%v",err,want)}
   if !want && !errors.Is(err,ErrCaseStaffChannelUnsafe){t.Fatalf("unexpected error: %v",err)}
  })
 }
 if err:=validateCaseStaffChannel(g,ch,nil,"bot",[]string{"bot-role"});err!=nil{t.Fatal(err)}
 assert("everyone deny missing",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites=c.PermissionOverwrites[1:]
 },false)
 assert("everyone explicitly allowed",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites[0].Allow=discordgo.PermissionViewChannel
 },false)
 assert("cross guild",func(_ *discordgo.Guild,c *discordgo.Channel){c.GuildID="foreign"},false)
 assert("voice channel",func(_ *discordgo.Guild,c *discordgo.Channel){c.Type=discordgo.ChannelTypeGuildVoice},false)
 assert("verified role bypass",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites=append(c.PermissionOverwrites,
   &discordgo.PermissionOverwrite{ID:"verified",Type:discordgo.PermissionOverwriteTypeRole,Allow:discordgo.PermissionViewChannel})
 },false)
 assert("staff management role",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites=append(c.PermissionOverwrites,
   &discordgo.PermissionOverwrite{ID:"staff",Type:discordgo.PermissionOverwriteTypeRole,Allow:discordgo.PermissionViewChannel})
 },true)
 assert("foreign member visibility",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites=append(c.PermissionOverwrites,
   &discordgo.PermissionOverwrite{ID:"other",Type:discordgo.PermissionOverwriteTypeMember,Allow:discordgo.PermissionViewChannel})
 },false)
 assert("bot cannot read history",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites[1].Deny=discordgo.PermissionReadMessageHistory
 },false)
 assert("bot cannot embed",func(_ *discordgo.Guild,c *discordgo.Channel){
  c.PermissionOverwrites[1].Deny=discordgo.PermissionEmbedLinks
 },false)
 assert("everyone administrator",func(g *discordgo.Guild,_ *discordgo.Channel){
  g.Roles[0].Permissions|=discordgo.PermissionAdministrator
 },false)
}

func TestCaseStaffCategoryInheritanceFailsClosed(t *testing.T){
 g,ch:=privateCaseFixture()
 parent:=&discordgo.Channel{ID:"category",GuildID:"guild",Type:discordgo.ChannelTypeGuildCategory,
  PermissionOverwrites:ch.PermissionOverwrites}
 ch.ParentID="category"
 ch.PermissionOverwrites=nil
 if err:=validateCaseStaffChannel(g,ch,parent,"bot",[]string{"bot-role"});!errors.Is(err,ErrCaseStaffChannelUnsafe){t.Fatal("a private parent without target-channel overrides is insufficient")}
 parent.GuildID="foreign"
 if err:=validateCaseStaffChannel(g,ch,parent,"bot",[]string{"bot-role"});!errors.Is(err,ErrCaseStaffChannelUnsafe){
  t.Fatal("foreign parent must not supply staff permissions")
 }
}
