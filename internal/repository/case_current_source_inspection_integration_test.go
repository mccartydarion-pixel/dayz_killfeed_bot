//go:build integration

package repository

import (
 "context"
 "fmt"
 "os"
 "strings"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/database"
)

// Writes below are restricted to a fresh disposable schema. The inspected
// repository performs only SELECTs and never reads production private data.
func TestCurrentSourceEvidenceInspectionScopeHistoryAndBound(t *testing.T) {
 url:=strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
 if url=="" {
  if os.Getenv("REQUIRE_INTEGRATION_DB")=="1"{t.Fatal("disposable integration database required")}
  t.Skip("TEST_DATABASE_URL unavailable")
 }
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true"{t.Fatal("disposable database authorization required")}
 ctx,cancel:=context.WithTimeout(context.Background(),3*time.Minute)
 defer cancel()
 admin,err:=database.Connect(ctx,url)
 if err!=nil{t.Fatal(err)}
 schema:=fmt.Sprintf("case_current_source_%d",time.Now().UnixNano())
 if _,err=admin.Pool.Exec(ctx,"CREATE SCHEMA "+schema);err!=nil{admin.Close();t.Fatal(err)}
 t.Cleanup(func(){_,_=admin.Pool.Exec(context.Background(),"DROP SCHEMA IF EXISTS "+schema+" CASCADE");admin.Close()})
 sep:="?"
 if strings.Contains(url,"?"){sep="&"}
 db,err:=database.Connect(ctx,url+sep+"search_path="+schema)
 if err!=nil{t.Fatal(err)}
 t.Cleanup(db.Close)
 if err=db.Migrate(ctx);err!=nil{t.Fatal(err)}
 one:=func(query string,args ...any)int64{
  t.Helper();var id int64
  if e:=db.Pool.QueryRow(ctx,query,args...).Scan(&id);e!=nil{t.Fatal(e)}
  return id
 }
 user:=one(`INSERT INTO app_users(discord_user_id,discord_username)
 VALUES('case-source-inspector','fixture') RETURNING id`)
 org:=one(`INSERT INTO organizations(name,slug,owner_user_id)
 VALUES('Case source fixture','case-source-fixture',$1) RETURNING id`,user)
 guild:=one(`INSERT INTO guilds(discord_guild_id) VALUES('case-source-guild') RETURNING id`)
 server:=one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','source-one','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`,guild,org)
 otherServer:=one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','source-two','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`,guild,org)
 source:="test/source-selected.ADM"
 oldSource:="test/source-previous.ADM"
 hash:=strings.Repeat("a",64)
 add:=func(g,s int64,src string,offset int64)int64{
  t.Helper()
  return one(`INSERT INTO case_evidence_events(guild_id,server_id,source_id,source_end_offset,line_sha256,event_type)
  VALUES($1,$2,$3,$4,$5,'PLAYER_HIT') RETURNING id`,g,s,src,offset,hash)
 }
 reader:=NewCurrentSourceEvidenceInspector(db.Pool)
 empty,err:=reader.Inspect(ctx,guild,server,source,2)
 if err!=nil||empty.SelectedSourceObservations!=0||empty.LatestRetainedMatchesSelected||
  empty.LatestRetainedSourceRef!=""||empty.LatestRetainedOffset!=nil{
  t.Fatalf("empty misreported: %+v %v",empty,err)
 }
 add(guild,server,source,100)
 add(guild,server,source,200)
 add(guild,server,oldSource,300)
 add(guild,otherServer,source,900)
 historical,err:=reader.Inspect(ctx,guild,server,source,1)
 if err!=nil||historical.SelectedSourceRef!=currentSourceRef(source)||
  historical.LatestRetainedSourceRef!=currentSourceRef(oldSource)||
  historical.LatestRetainedMatchesSelected||
  historical.SelectedSourceObservations!=1||!historical.SelectedSourceWindowTruncated||
  historical.LatestSelectedOffset==nil||*historical.LatestSelectedOffset!=200||
  historical.LatestRetainedOffset==nil||*historical.LatestRetainedOffset!=300||
  historical.LatestSelectedIngestedAt==nil||historical.LatestRetainedIngestedAt==nil{
  t.Fatalf("older current-source rows were promoted: %+v %v",historical,err)
 }
 // Even though a historical selected-source row exists, newer retained rows
 // from another boot prevent a false latest-source match.
 add(guild,server,source,400)
 current,err:=reader.Inspect(ctx,guild,server,source,2)
 if err!=nil||!current.LatestRetainedMatchesSelected||
  current.SelectedSourceObservations!=2||!current.SelectedSourceWindowTruncated||
  current.LatestRetainedOffset==nil||*current.LatestRetainedOffset!=400||
  current.LatestSelectedOffset==nil||*current.LatestSelectedOffset!=400{
  t.Fatalf("legitimate selected source mismatch: %+v %v",current,err)
 }
 foreign,err:=reader.Inspect(ctx,guild,otherServer,source,2)
 if err!=nil||foreign.SelectedSourceObservations!=1||
  foreign.LatestRetainedOffset==nil||*foreign.LatestRetainedOffset!=900{
  t.Fatalf("other server scope: %+v %v",foreign,err)
 }
 absent,err:=reader.Inspect(ctx,guild+1000000,server,source,2)
 if err!=nil||absent.SelectedSourceObservations!=0||absent.LatestRetainedOffset!=nil{
  t.Fatalf("foreign guild leaked: %+v %v",absent,err)
 }
 for _,bad:=range []struct{g,s int64;source string;limit int}{
  {0,server,source,1},{guild,0,source,1},{guild,server,"",1},
  {guild,server," "+source,1},{guild,server,source,0},{guild,server,source,201},
 }{
  if _,err:=reader.Inspect(ctx,bad.g,bad.s,bad.source,bad.limit);err==nil{
   t.Fatalf("accepted invalid scope/bound: %+v",bad)
  }
 }
 var nilReader *CurrentSourceEvidenceInspector
 if _,err:=nilReader.Inspect(ctx,guild,server,source,1);err==nil{t.Fatal("nil repository accepted")}
}
