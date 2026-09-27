package main

import "testing"

func TestQASetupRequiresExactServiceAndPrivateDatabase(t *testing.T){
 good:="postgres://test:test@postgres-bqu0.railway.internal:5432/railway?sslmode=disable"
 cases:=[]struct{name,project,service,gate,dsn,expectedHost string;accepted bool}{
  {"exact staging fixture",qaProjectID,qaServiceID,"1",good,"postgres-bqu0.railway.internal",true},
  {"mismatched private target",qaProjectID,qaServiceID,"1",good,"other.railway.internal",false},
  {"wrong project","68116994-0ea2-4c75-9cb1-2c7061b8a559",qaServiceID,"1",good,"postgres-bqu0.railway.internal",false},
  {"wrong service",qaProjectID,"another-service","1",good,"postgres-bqu0.railway.internal",false},
  {"missing gate",qaProjectID,qaServiceID,"",good,"postgres-bqu0.railway.internal",false},
  {"public host",qaProjectID,qaServiceID,"1","postgres://test:test@public.proxy.rlwy.net:5432/railway","postgres-bqu0.railway.internal",false},
  {"missing URL",qaProjectID,qaServiceID,"1","","postgres-bqu0.railway.internal",false},
  {"malformed URL",qaProjectID,qaServiceID,"1","not-a-postgres-url","postgres-bqu0.railway.internal",false},
 }
 for _,tc:=range cases{
  t.Run(tc.name,func(t *testing.T){
   err:=authorize(tc.project,tc.service,tc.gate,tc.dsn,tc.expectedHost)
   if (err==nil)!=tc.accepted{t.Fatalf("authorization result mismatch, accepted=%v err=%v",tc.accepted,err)}
  })
 }
}
