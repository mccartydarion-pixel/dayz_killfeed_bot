// Command case-discord-qa performs a strictly opt-in one-shot Discord delivery
// probe using a separate QA bot and either a private QA guild or a dedicated
// #case-qa channel in an existing guild. It is NEVER started by
// cmd/server or any production worker. No Stripe, Nitrado or player data.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	championdiscord "github.com/yourname/dayz-killfeed/internal/discord"
)

const (
	modePreflight = "preflight"
	modeSend = "send-once"
	modeVerify = "verify-existing"
	sendConfirmation = "SEND_ONE_SYNTHETIC_QA_MESSAGE"
	sendEnvironment = "YES_ONE_SYNTHETIC_QA_MESSAGE"
	existingGuildConfirmation = "USE_EXISTING_GUILD_PRIVATE_CASE_QA"
	existingGuildEnvironment = "YES_EXISTING_GUILD_PRIVATE_CASE_QA"
	qaTitle = "C.A.S.E. QA • SYNTHETIC DELIVERY PROBE"
	qaDescription = "QA-only synthetic Discord transport check. No DayZ player data, evidence, billing, cheat finding, detector or enforcement."
)

var snowflake = regexp.MustCompile(`^[1-9][0-9]{14,21}$`)
var referencePattern = regexp.MustCompile(`^CHAMPION-CASE-QA-[A-F0-9]{24}$`)

type probeConfig struct {
	mode, guildID, channelID, botID string
	productionGuildID, productionBotID, productionChannels string
	confirm, existingGuildConfirm, messageID, reference string
	useExistingGuild bool
	lostAckSimulation bool
}

func (c probeConfig) validate() error {
	if c.mode!=modePreflight && c.mode!=modeSend && c.mode!=modeVerify {
		return errors.New("mode must be preflight, send-once or verify-existing")
	}
	if !snowflake.MatchString(c.guildID) || !snowflake.MatchString(c.channelID) || !snowflake.MatchString(c.botID) {
		return errors.New("QA guild, channel and expected QA bot must be valid Discord snowflake IDs")
	}
	if c.useExistingGuild && (!snowflake.MatchString(c.productionGuildID) || c.guildID!=c.productionGuildID) {
		return errors.New("existing-guild mode requires the exact production guild ID")
	}
	if c.lostAckSimulation && c.mode!=modeSend {
		return errors.New("lost-ack simulation requires send-once mode")
	}
	if c.mode==modeSend {
		if !snowflake.MatchString(c.productionGuildID) || !snowflake.MatchString(c.productionBotID) {
			return errors.New("production guild and bot IDs must be supplied as independent denylist guards")
		}
		if c.botID==c.productionBotID {
			return errors.New("refusing to send: QA bot matches the production bot")
		}
		if c.useExistingGuild {
			if c.guildID!=c.productionGuildID {
				return errors.New("existing-guild mode requires the exact declared production guild")
			}
			if c.existingGuildConfirm!=existingGuildConfirmation ||
				os.Getenv("CASE_DISCORD_QA_EXISTING_GUILD")!=existingGuildEnvironment {
				return errors.New("existing-guild mode requires separate explicit CLI and environment approval")
			}
			channels:=strings.Split(c.productionChannels,",")
			if len(channels)==0 || strings.TrimSpace(c.productionChannels)=="" {
				return errors.New("existing-guild mode requires a production channel denylist")
			}
			for _,id:=range channels {
				id=strings.TrimSpace(id)
				if !snowflake.MatchString(id) {return errors.New("production channel denylist contains an invalid Discord ID")}
				if id==c.channelID {return errors.New("refusing to send: QA target is a live production channel")}
			}
		} else if c.guildID==c.productionGuildID {
			return errors.New("production guild requires explicit existing-guild mode")
		}
		if c.confirm!=sendConfirmation || os.Getenv("CASE_DISCORD_QA_ALLOW_SEND")!=sendEnvironment {
			return errors.New("send blocked: explicit CLI confirmation and environment authorization are both required")
		}
	}
	if c.mode==modeVerify && (!snowflake.MatchString(c.messageID) || !referencePattern.MatchString(c.reference)) {
		return errors.New("verify-existing needs the exact message snowflake and a CHAMPION-CASE-QA reference")
	}
	return nil
}

func newQAReference() (string,error) {
	var random [12]byte
	if _,err:=rand.Read(random[:]);err!=nil{return "",errors.New("could not create QA reference")}
	return "CHAMPION-CASE-QA-"+strings.ToUpper(hex.EncodeToString(random[:])),nil
}

func qaEmbed(reference string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:qaTitle,
		Description:qaDescription,
		Fields:[]*discordgo.MessageEmbedField{
			{Name:"QA reference",Value:reference},
			{Name:"Data",Value:"Synthetic only · 0 player records"},
			{Name:"Action",Value:"Transport test only · no enforcement"},
		},
		Footer:&discordgo.MessageEmbedFooter{Text:"CHAMPION C.A.S.E. • ISOLATED QA • NOT LIVE EVIDENCE"},
	}
}

func matchesQAProbe(msg *discordgo.Message,botID,channelID,reference string) bool {
	if msg==nil || msg.ID=="" || msg.ChannelID!=channelID ||
		msg.Author==nil || msg.Author.ID!=botID || !referencePattern.MatchString(reference) {
		return false
	}
	for _,e:=range msg.Embeds {
		if e==nil || e.Title!=qaTitle || e.Description!=qaDescription {continue}
		for _,f:=range e.Fields {
			if f!=nil && f.Name=="QA reference" && f.Value==reference {return true}
		}
	}
	return false
}

func main() {
	cfg:=probeConfig{}
	flag.StringVar(&cfg.mode,"mode",modePreflight,"preflight (no send), send-once, verify-existing")
	flag.StringVar(&cfg.guildID,"guild","","QA Discord guild ID, including existing Champions guild in guarded mode")
	flag.StringVar(&cfg.channelID,"channel","","private QA text channel ID")
	flag.StringVar(&cfg.botID,"bot","","expected QA bot user ID")
	flag.StringVar(&cfg.productionGuildID,"production-guild","","live guild ID (mandatory for send)")
	flag.StringVar(&cfg.productionBotID,"production-bot","","live bot user ID to deny (mandatory for send)")
	flag.StringVar(&cfg.productionChannels,"production-channels","","comma-separated live output channel IDs, mandatory when using an existing guild")
	flag.BoolVar(&cfg.useExistingGuild,"use-existing-guild",false,"allow isolated private #case-qa inside the existing guild; never the production bot")
	flag.StringVar(&cfg.existingGuildConfirm,"existing-guild-confirm","","separate confirmation when targeting an existing guild")
	flag.StringVar(&cfg.confirm,"confirm","","exact send confirmation")
	flag.StringVar(&cfg.messageID,"message","","Discord message ID for read-only verification")
	flag.StringVar(&cfg.reference,"reference","","QA reference for read-only verification")
	flag.BoolVar(&cfg.lostAckSimulation,"simulate-lost-ack",false,"send once then intentionally discard the response identity; NEVER resend")
	flag.Parse()
	if err:=run(cfg);err!=nil{
		fmt.Fprintln(os.Stderr,"C.A.S.E. QA blocked:",err)
		os.Exit(2)
	}
}

func run(cfg probeConfig) error {
	if err:=cfg.validate();err!=nil{return err}
	token:=os.Getenv("CASE_DISCORD_QA_BOT_TOKEN")
	if strings.TrimSpace(token)=="" {return errors.New("QA bot token is not configured in CASE_DISCORD_QA_BOT_TOKEN")}
	ctx,cancel:=context.WithTimeout(context.Background(),40*time.Second)
	defer cancel()
	client,err:=championdiscord.New(token)
	if err!=nil{return errors.New("could not initialize QA Discord client")}
	session:=client.Session()
	if session==nil{return errors.New("QA Discord session is unavailable")}
	if session.Client==nil{session.Client=&http.Client{Timeout:12*time.Second}} else {session.Client.Timeout=12*time.Second}
	bot,err:=session.User("@me")
	if err!=nil || bot==nil || bot.ID==""{return errors.New("could not verify QA bot identity through Discord")}
	if bot.ID!=cfg.botID {return errors.New("authenticated bot ID does not match the explicitly expected QA bot ID")}
	if session.State==nil{session.State=discordgo.NewState()}
	session.State.User=bot
	// Synthetic QA checks only the bot's transport permissions and the exact
	// QA channel. Staff-member privacy review remains a separate, unchanged
	// requirement for real Watch digests.
	if err:=client.VerifyCaseSyntheticQAChannel(ctx,cfg.guildID,cfg.channelID);err!=nil{
		return fmt.Errorf("synthetic QA channel preflight failed (no message sent): %w",err)
	}
	fmt.Printf("QA preflight passed: guild=%s channel=%s bot=%s; probe is limited to this channel.\n",
		cfg.guildID,cfg.channelID,cfg.botID)
	if cfg.mode==modePreflight {fmt.Println("READ-ONLY: no message was sent.");return nil}
	if cfg.mode==modeVerify {
		message,err:=session.ChannelMessage(cfg.channelID,cfg.messageID)
		if err!=nil{return errors.New("Discord could not fetch the exact QA message; nothing sent")}
		if !matchesQAProbe(message,cfg.botID,cfg.channelID,cfg.reference){
			return errors.New("message author, channel, title, description or QA reference did not match")
		}
		fmt.Printf("VERIFIED EXISTING QA MESSAGE: channel=%s message=%s reference=%s; no send.\n",
			cfg.channelID,cfg.messageID,cfg.reference)
		return nil
	}
	ref,err:=newQAReference()
	if err!=nil{return err}
	fmt.Printf("ABOUT TO SEND ONE SYNTHETIC QA MESSAGE: reference=%s\n",ref)
	// Exactly one send invocation. There is no retry, loop, or production
	// fallback. If the response is lost the operator finds it by reference.
	msg,sendErr:=session.ChannelMessageSendComplex(cfg.channelID,
		&discordgo.MessageSend{Embeds:[]*discordgo.MessageEmbed{qaEmbed(ref)}})
	if sendErr!=nil || msg==nil || msg.ID=="" {
		return fmt.Errorf("send outcome unconfirmed; search QA channel for reference %s; DO NOT automatically resend",ref)
	}
	if cfg.lostAckSimulation {
		// Discord returned an ACK successfully. Intentionally discard its
		// identity to exercise the human UNKNOWN runbook, not to claim an
		// actual transport failure. Exactly one message was sent.
		fmt.Printf("SIMULATED LOST ACK AFTER A SUCCESSFUL SEND: reference=%s. Inspect the QA channel; do not re-run send-once for this reference.\n",ref)
		return nil
	}
	read,err:=session.ChannelMessage(cfg.channelID,msg.ID)
	if err!=nil || !matchesQAProbe(read,cfg.botID,cfg.channelID,ref) {
		return fmt.Errorf("Discord read-back unconfirmed for reference %s and message %s; DO NOT automatically resend",ref,msg.ID)
	}
	fmt.Printf("SINGLE QA DELIVERY VERIFIED: guild=%s channel=%s message=%s reference=%s\n",
		cfg.guildID,cfg.channelID,msg.ID,ref)
	return nil
}
