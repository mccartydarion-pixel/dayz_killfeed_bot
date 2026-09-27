package discord

import (
 "encoding/json"
 "strings"
 "testing"
 "time"
)

func TestCASEStaffDemoIsClearlySyntheticAndNonEnforcing(t *testing.T) {
 embed:=BuildCASEStaffDemoEmbed()
 if embed==nil||embed.Footer==nil {t.Fatal("demo card missing")}
 raw,err:=json.Marshal(embed)
 if err!=nil {t.Fatal(err)}
 text:=string(raw)
 for _,required:=range []string{
  "DEMO ONLY","SYNTHETIC PREVIEW","NOT A REAL PLAYER OR DETECTION",
  "DEMO SERVER (fixture only)","CASE-MOV-001","BLOCKED",
  "not full ADM coverage","Safe speed pairs","DISABLED",
  "no alert, accusation, case, score, or sanction","NOT LIVE EVIDENCE",
 }{
  if !strings.Contains(text,required) {t.Fatalf("demo omitted %q",required)}
 }
 for _,prohibited:=range []string{
  "CONFIRMED CHEATER","BAN PLAYER","HIGH RISK","SUSPICIOUS MOVEMENT",
  "https://discord.com/api/webhooks/","@everyone",
 }{
  if strings.Contains(strings.ToUpper(text),strings.ToUpper(prohibited)){
   t.Fatalf("demo contains prohibited claim %q",prohibited)
  }
 }
 stamp,err:=time.Parse(time.RFC3339Nano,embed.Timestamp)
 if err!=nil||!stamp.Equal(time.Date(2026,9,27,12,0,0,0,time.UTC)) {
  t.Fatalf("demo timestamp must be stable fixture time, got %q: %v",embed.Timestamp,err)
 }
 if len(embed.Fields)!=8 {t.Fatalf("unexpected demo fields %d",len(embed.Fields))}
 // Builder always returns a fresh object. Mutating a preview cannot affect
 // a subsequent preview or turn it into a live message template.
 embed.Title="Fake live alert"
 fresh:=BuildCASEStaffDemoEmbed()
 if fresh.Title==embed.Title {t.Fatal("preview state leaked between calls")}
}
