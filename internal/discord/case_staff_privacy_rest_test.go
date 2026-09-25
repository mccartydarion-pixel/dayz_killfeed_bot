package discord

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/bwmarrin/discordgo"
)

// Discord's GET guild member endpoint requires /members/{user.id}, not
// /members/@me. Exercise actual discordgo HTTP routing without a real token.
func TestVerifyCaseStaffChannelFetchesBotMemberByRealUserID(t *testing.T) {
    guild,channel:=privateCaseFixture()
    var memberCalls int
    server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Content-Type","application/json")
        switch r.URL.Path {
        case "/channels/staff-channel":
            _=json.NewEncoder(w).Encode(channel)
        case "/guilds/guild":
            _=json.NewEncoder(w).Encode(guild)
        case "/guilds/guild/members/bot":
            memberCalls++
            _=json.NewEncoder(w).Encode(&discordgo.Member{
                GuildID:"guild",User:&discordgo.User{ID:"bot"},Roles:[]string{"bot-role"},
            })
        default:
            t.Errorf("unexpected Discord REST path %s (especially @me on guild-member GET)",r.URL.Path)
            http.NotFound(w,r)
        }
    }))
    defer server.Close()
    oldGuilds,oldChannels:=discordgo.EndpointGuilds,discordgo.EndpointChannels
    discordgo.EndpointGuilds=server.URL+"/guilds/"
    discordgo.EndpointChannels=server.URL+"/channels/"
    defer func(){discordgo.EndpointGuilds=oldGuilds;discordgo.EndpointChannels=oldChannels}()

    client,err:=New("fake-qa-token")
    if err!=nil{t.Fatal(err)}
    client.session.State.User=&discordgo.User{ID:"bot"}
    if err:=client.VerifyCaseStaffChannel(context.Background(),"guild","staff-channel");err!=nil{
        t.Fatalf("explicitly private QA channel failed: %v",err)
    }
    if memberCalls!=1{t.Fatalf("expected exactly one bot-ID membership GET, got %d",memberCalls)}
}

func TestCaseStaffPrivacyDiagnosticIdentifiesPolicyFailure(t *testing.T){
    guild,channel:=privateCaseFixture()
    channel.PermissionOverwrites=channel.PermissionOverwrites[1:]
    err:=validateCaseStaffChannel(guild,channel,nil,"bot",[]string{"bot-role"})
    if err==nil || !strings.Contains(err.Error(),"explicitly deny @everyone"){
        t.Fatalf("missing @everyone deny should name actionable setting: %v",err)
    }
    guild,channel=privateCaseFixture()
    guild.Roles[1].Permissions &^= discordgo.PermissionReadMessageHistory
    channel.PermissionOverwrites[1].Allow &^=discordgo.PermissionReadMessageHistory
    err=validateCaseStaffChannel(guild,channel,nil,"bot",[]string{"bot-role"})
    if err==nil || !strings.Contains(err.Error(),"Read Message History"){
        t.Fatalf("missing bot history access should be explicit: %v",err)
    }
}

func TestSyntheticQATransportAllowsOperatorMemberWithoutWeakeningRealStaffGate(t *testing.T) {
    guild,channel:=privateCaseFixture()
    channel.Name="case-qa"
    // An operator directly allowed to view the QA channel must not block a
    // synthetic transport test; the production Watch gate still rejects it.
    channel.PermissionOverwrites=append(channel.PermissionOverwrites,
        &discordgo.PermissionOverwrite{ID:"operator",Type:discordgo.PermissionOverwriteTypeMember,Allow:discordgo.PermissionViewChannel})
    if err:=validateCaseStaffChannel(guild,channel,nil,"bot",[]string{"bot-role"});err==nil{
        t.Fatal("real Watch privacy check must still reject individual member allow")
    }
    var memberCalls int
    server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Content-Type","application/json")
        switch r.URL.Path {
        case "/channels/staff-channel":
            _=json.NewEncoder(w).Encode(channel)
        case "/guilds/guild":
            _=json.NewEncoder(w).Encode(guild)
        case "/guilds/guild/members/bot":
            memberCalls++
            _=json.NewEncoder(w).Encode(&discordgo.Member{
                GuildID:"guild",User:&discordgo.User{ID:"bot"},Roles:[]string{"bot-role"},
            })
        default:
            t.Errorf("unexpected QA REST path %s",r.URL.Path)
            http.NotFound(w,r)
        }
    }))
    defer server.Close()
    oldGuilds,oldChannels:=discordgo.EndpointGuilds,discordgo.EndpointChannels
    discordgo.EndpointGuilds=server.URL+"/guilds/"
    discordgo.EndpointChannels=server.URL+"/channels/"
    defer func(){discordgo.EndpointGuilds=oldGuilds;discordgo.EndpointChannels=oldChannels}()
    client,err:=New("fake-qa-token")
    if err!=nil{t.Fatal(err)}
    client.session.State.User=&discordgo.User{ID:"bot"}
    check:=func() error{return client.VerifyCaseSyntheticQAChannel(context.Background(),"guild","staff-channel")}
    if err:=check();err!=nil{t.Fatalf("valid synthetic QA test blocked by operator member: %v",err)}
    if memberCalls!=1{t.Fatalf("expected one numeric bot guild member GET, got %d",memberCalls)}
    channel.Name="killfeed"
    if err:=check();err==nil || !strings.Contains(err.Error(),"exact case-qa"){
        t.Fatalf("wrong output channel was accepted: %v",err)
    }
    channel.Name="case-qa"
    channel.PermissionOverwrites[1].Allow &^=discordgo.PermissionSendMessages
    guild.Roles[1].Permissions &^=discordgo.PermissionSendMessages
    if err:=check();err==nil || !strings.Contains(err.Error(),"Send Messages"){
        t.Fatalf("QA bot without send permission was accepted: %v",err)
    }
}
