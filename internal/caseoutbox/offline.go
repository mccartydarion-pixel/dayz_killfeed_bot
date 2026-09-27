// Package caseoutbox models offline-only delivery state transitions.
//
// This package has no database, Discord client, scheduler, or production caller.
// A valid synthetic entry is not authorization to send a message.
package caseoutbox

import (
 "errors"
 "fmt"
 "strings"
 "time"
)

type Status string
const (
 Pending Status = "PENDING"
 Leased Status = "LEASED"
 RetryWait Status = "RETRY_WAIT"
 Sent Status = "SENT"
 Dead Status = "DEAD"
 Suppressed Status = "SUPPRESSED"
)

const MaxAttempts = 5
const LeaseDuration = 30 * time.Second

type Scope struct { GuildID, InstallationID, ServerID int64 }
type Item struct {
 Key string
 Scope Scope
 Status Status
 Attempts int
 LeaseVersion uint64
 LeaseUntil time.Time
 NextAt time.Time
 SentAt time.Time
 LastErrorCode string // approved class only; no raw Discord/API error or player data
}

func NewSynthetic(key string, scope Scope, at time.Time) (Item,error) {
 if len(key)!=64 || strings.Trim(key,"0123456789abcdef")!="" ||
  scope.GuildID<=0 || scope.InstallationID<=0 || scope.ServerID<=0 || at.IsZero() {
  return Item{},errors.New("invalid synthetic outbox identity")
 }
 return Item{Key:key,Scope:scope,Status:Pending,NextAt:at.UTC()},nil
}

type Lease struct { Version uint64; Until time.Time }

func Acquire(item Item, now time.Time) (Item,Lease,error) {
 if now.IsZero() {return item,Lease{},errors.New("missing time")}
 now=now.UTC()
 if item.Status==Leased {
  if now.Before(item.LeaseUntil) {return item,Lease{},errors.New("existing lease is active")}
  // Expiry never means delivery failed: an earlier request may have succeeded
  // remotely but not yet acknowledged. A durable implementation must reconcile
  // its idempotency key before any new network attempt.
  return item,Lease{},errors.New("expired lease requires delivery reconciliation")
 }
 if item.Status!=Pending && item.Status!=RetryWait {return item,Lease{},errors.New("item not claimable")}
 if now.Before(item.NextAt) {return item,Lease{},errors.New("retry not yet due")}
 if item.Attempts>=MaxAttempts {return item,Lease{},errors.New("attempt budget exhausted")}
 item.Status=Leased
 item.Attempts++
 item.LeaseVersion++
 item.LeaseUntil=now.Add(LeaseDuration)
 return item,Lease{Version:item.LeaseVersion,Until:item.LeaseUntil},nil
}

func matching(item Item, lease Lease, at time.Time) bool {
 return !at.IsZero() && item.Status==Leased && lease.Version>0 &&
  lease.Version==item.LeaseVersion && lease.Until.Equal(item.LeaseUntil) &&
  !at.UTC().After(item.LeaseUntil)
}

func Ack(item Item, lease Lease, at time.Time) (Item,error) {
 if !matching(item,lease,at) {return item,errors.New("invalid or stale acknowledgement")}
 item.Status=Sent
 item.SentAt=at.UTC()
 item.LeaseUntil=time.Time{}
 return item,nil
}

var allowedFailureCodes=map[string]bool{
 "RATE_LIMIT":true,"TEMPORARY_TRANSPORT":true,"DISCORD_UNAVAILABLE":true,
 "DESTINATION_UNAVAILABLE":true,"PERMISSION_REVOKED":true,
}

func Failure(item Item,lease Lease,at time.Time,code string) (Item,error) {
 if !matching(item,lease,at) || !allowedFailureCodes[code] {
  return item,errors.New("invalid failure receipt or code")
 }
 item.LastErrorCode=code
 item.LeaseUntil=time.Time{}
 // Route and access failures require owner repair, never silent reassignment.
 if code=="DESTINATION_UNAVAILABLE"||code=="PERMISSION_REVOKED"||item.Attempts>=MaxAttempts {
  item.Status=Dead
  item.NextAt=time.Time{}
  return item,nil
 }
 item.Status=RetryWait
 // Bounded exponential backoff: 30, 60, 120, 240 seconds.
 wait:=30*time.Second*time.Duration(1<<uint(item.Attempts-1))
 if wait>4*time.Minute {wait=4*time.Minute}
 item.NextAt=at.UTC().Add(wait)
 return item,nil
}

func Suppress(item Item,reason string) (Item,error) {
 if reason!="STAFF_DISMISSED" && reason!="OWNER_DISABLED" && reason!="EVIDENCE_INVALIDATED" {
  return item,errors.New("invalid suppression reason")
 }
 if item.Status==Sent||item.Status==Dead||item.Status==Suppressed {
  return item,fmt.Errorf("terminal item cannot be suppressed: %s",item.Status)
 }
 item.Status=Suppressed
 item.LeaseVersion++ // invalidate any in-flight receipt
 item.LeaseUntil=time.Time{}
 item.NextAt=time.Time{}
 item.LastErrorCode=reason
 return item,nil
}
