//go:build integration

package main

import (
 "context"
 "fmt"
 "os"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/database"
)

// Only the disposable CI database is used; Railway staging and production
// variables are never read or inherited into the connection.
func TestQASyntheticFixtureOnDisposablePostgres(t *testing.T){
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true"||os.Getenv("TEST_DATABASE_URL")==""{
  t.Fatal("explicit disposable database gate required")
 }
 ctx,cancel:=context.WithTimeout(context.Background(),90*time.Second);defer cancel()
 db,err:=database.Connect(ctx,os.Getenv("TEST_DATABASE_URL"))
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 if err:=db.Migrate(ctx);err!=nil{t.Fatal(err)}
 g,s,err:=seedSynthetic(ctx,db.Pool)
 if err!=nil{t.Fatalf("fixture first seed: %v",err)}
 g2,s2,err:=seedSynthetic(ctx,db.Pool)
 if err!=nil||g2!=g||s2!=s{t.Fatalf("fixture not idempotent: %d %d %v",g2,s2,err)}
 var n int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&n);err!=nil||n!=2{
  t.Fatalf("unexpected synthetic evidence count %d: %v",n,err)
 }
 var evaluations int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&evaluations);err!=nil||evaluations!=0{
  t.Fatalf("setup created a diagnostic %d: %v",evaluations,err)
 }
 // A prior fixture with altered source content must not be accepted as valid.
 _,err=db.Pool.Exec(ctx,`UPDATE case_evidence_events SET line_sha256=$1
 WHERE guild_id=$2 AND server_id=$3 AND source_id=$4 AND source_end_offset=110`,
 fmt.Sprintf("%064x",9),g,s,qaSource)
 if err!=nil{t.Fatal(err)}
 if _,_,err:=seedSynthetic(ctx,db.Pool);err==nil{t.Fatal("altered synthetic source content accepted")}
}
