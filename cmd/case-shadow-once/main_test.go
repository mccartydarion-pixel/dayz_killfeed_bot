package main

import (
 "context"
 "strings"
 "testing"
)

func validPreview() options {
 return options{mode:"preview",guild:11,server:22,limit:2}
}

func TestOneShotPreviewHasNoWriteGate(t *testing.T){
 o:=validPreview()
 if err:=validate(o,"");err!=nil{t.Fatalf("preview rejected: %v",err)}
 // No DB URL and no writes: validation succeeds, the command fails closed
 // before any connection or evaluation.
 if err:=run(context.Background(),o,"","","");err==nil{t.Fatal("missing DB URL accepted")}
}
func TestOneShotExecutionRequiresSeparateExplicitGates(t *testing.T){
 o:=validPreview();o.mode="execute"
 if err:=validate(o,"");err==nil{t.Fatal("missing environment gate accepted")}
 if err:=validate(o,"1");err==nil{t.Fatal("missing acknowledgement accepted")}
 o.ack=executeAcknowledgment
 if err:=validate(o,"1");err==nil{t.Fatal("missing exact preflight fingerprint accepted")}
 o.expected=strings.Repeat("a",64)
 if err:=validate(o,"1");err==nil{t.Fatal("missing source provenance plan accepted")}
 o.expectedPlan=strings.Repeat("b",64)
 if err:=validate(o,"1");err!=nil{t.Fatalf("fully gated plan rejected: %v",err)}
 for _,fp:=range []string{"",strings.Repeat("A",64),strings.Repeat("g",64),strings.Repeat("a",63)} {
  o.expected=fp
  if err:=validate(o,"1");err==nil{t.Fatalf("invalid fingerprint accepted: %q",fp)}
 }
}
func TestOneShotBoundariesAndReadOnlyMode(t *testing.T){
 o:=validPreview()
 for _,limit:=range []int{0,51,-1}{
  o.limit=limit
  if err:=validate(o,"");err==nil{t.Fatalf("invalid limit accepted: %d",limit)}
 }
 o=validPreview();o.guild=0
 if err:=validate(o,"");err==nil{t.Fatal("missing guild accepted")}
 o=validPreview();o.server=0
 if err:=validate(o,"");err==nil{t.Fatal("missing server accepted")}
 o=validPreview();o.mode="anything"
 if err:=validate(o,"");err==nil{t.Fatal("unknown mode accepted")}
}

func TestSourceProvenancePlanBindsExactContent(t *testing.T){
 a:=provenanceHash(11,22,"source-a",[]string{"1:100:aaa","2:200:bbb"})
 same:=provenanceHash(11,22,"source-a",[]string{"1:100:aaa","2:200:bbb"})
 if a!=same||len(a)!=64{t.Fatal("plan not deterministic")}
 for _,v:=range []string{
  provenanceHash(11,22,"source-b",[]string{"1:100:aaa","2:200:bbb"}),
  provenanceHash(11,23,"source-a",[]string{"1:100:aaa","2:200:bbb"}),
  provenanceHash(11,22,"source-a",[]string{"1:100:aaa","2:200:changed"}),
  provenanceHash(11,22,"source-a",[]string{"1:100:aaa","2:201:bbb"}),
  provenanceHash(11,22,"source-a",[]string{"2:200:bbb","1:100:aaa"}),
 }{
  if a==v{t.Fatal("different source provenance reused an operator plan")}
 }
}
