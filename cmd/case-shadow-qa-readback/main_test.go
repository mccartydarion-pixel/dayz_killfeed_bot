package main

import "testing"

func TestReadbackRefusesWrongContextAndPublicURL(t *testing.T){
 private:="postgres://reader:fake@qa-db.railway.internal:5432/railway?sslmode=disable"
 cases:=[]struct{name,p,s,d,url,host string;ok bool}{
 {"exact",projectID,serviceID,databaseServiceID,private,"qa-db.railway.internal",true},
 {"wrong project","production",serviceID,databaseServiceID,private,"qa-db.railway.internal",false},
 {"wrong service",projectID,"other",databaseServiceID,private,"qa-db.railway.internal",false},
 {"wrong database",projectID,serviceID,"other",private,"qa-db.railway.internal",false},
 {"mismatch host",projectID,serviceID,databaseServiceID,private,"other.railway.internal",false},
 {"public URL",projectID,serviceID,databaseServiceID,"postgres://reader:fake@public.rlwy.net:5432/railway","public.rlwy.net",false},
 {"empty URL",projectID,serviceID,databaseServiceID,"","qa-db.railway.internal",false},
 }
 for _,c:=range cases{t.Run(c.name,func(t *testing.T){
  err:=authorize(c.p,c.s,c.d,c.url,c.host)
  if (err==nil)!=c.ok{t.Fatalf("unexpected authorization: %v",err)}
 })}
}
