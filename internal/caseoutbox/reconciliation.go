package caseoutbox

import (
 "errors"
 "time"
)

// ReconciliationOutcome is an offline fixture assertion, NOT a trusted remote
// receipt. A live implementation must independently verify identity, scope,
// transport receipt and whether non-delivery can actually be established.
type ReconciliationOutcome string

const (
 DeliveryConfirmed ReconciliationOutcome = "DELIVERY_CONFIRMED"
 NonDeliveryConfirmed ReconciliationOutcome = "NON_DELIVERY_CONFIRMED"
 DeliveryUncertain ReconciliationOutcome = "DELIVERY_UNCERTAIN"
)

type ReconciliationProof struct {
 Key string
 Scope Scope
 LeaseVersion uint64
 ObservedAt time.Time
 Outcome ReconciliationOutcome
 Verified bool // synthetic fixture only; production must not trust this input
}

// ReconcileExpiredSynthetic is pure and has no production caller, Discord
// client, database, scheduler or route. An expired lease is never proof that a
// remote send failed. Ambiguity must remain leased, not silently retried.
func ReconcileExpiredSynthetic(item Item, lease Lease, proof ReconciliationProof) (Item,error) {
 if item.Status!=Leased||lease.Version==0||lease.Version!=item.LeaseVersion||
  !lease.Until.Equal(item.LeaseUntil)||proof.LeaseVersion!=lease.Version||
  proof.Key!=item.Key||proof.Scope!=item.Scope||!proof.Verified||
  proof.ObservedAt.IsZero()||proof.ObservedAt.UTC().Before(item.LeaseUntil) {
  return item,errors.New("reconciliation proof missing or stale")
 }
 switch proof.Outcome {
 case DeliveryConfirmed:
  // SentAt is the verification timestamp, not an asserted remote send time.
  item.Status=Sent
  item.SentAt=proof.ObservedAt.UTC()
  item.LeaseUntil=time.Time{}
  item.NextAt=time.Time{}
  item.LastErrorCode=""
  return item,nil
 case NonDeliveryConfirmed:
  // A real integration must establish non-delivery independently. Discord
  // does not provide a transactional exactly-once send guarantee.
  item.LeaseUntil=time.Time{}
  if item.Attempts>=MaxAttempts {
   item.Status=Dead
   item.NextAt=time.Time{}
  } else {
   item.Status=RetryWait
   item.NextAt=proof.ObservedAt.UTC().Add(30*time.Second)
  }
  item.LastErrorCode="VERIFIED_NON_DELIVERY"
  return item,nil
 case DeliveryUncertain:
  return item,errors.New("remote delivery remains ambiguous; manual reconciliation required")
 default:
  return item,errors.New("unknown reconciliation outcome")
 }
}
