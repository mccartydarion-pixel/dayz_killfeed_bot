package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrCaseCheckoutConflict = errors.New("C.A.S.E. checkout already exists or server is not eligible")
	ErrCaseWebhookMismatch = errors.New("C.A.S.E. webhook identity does not match pending subscription")
	ErrCaseTrialAlreadyUsed = errors.New("C.A.S.E. founder trial was already granted to this server")
)

type CaseCheckoutReservation struct {
	ID int64
	Attempt int64
	OrganizationID, InstallationID, GameServerID int64
	Tier, ProviderCustomerID, SessionID, CheckoutURL string
}

type CaseWebhookState struct {
	EventID, EventType string
	AddonID, OrganizationID, InstallationID, GameServerID int64
	Tier, CustomerID, SubscriptionID, PriceID, Status string
	CheckoutSessionID string
	CurrentPeriodStart, CurrentPeriodEnd, TrialStart, TrialEnd, PaidThrough *time.Time
	FounderTrialOffer bool
	CancelAtPeriodEnd bool
}

// ReserveCaseCheckout only creates a pending row when the installation,
// connected guild, game server, and organization match the same ownership
// chain. Once a row exists, every retry reuses its idempotency identity. A
// failed/canceled purchase never silently creates another paid Stripe object.
func (r *CaseAddonSubscriptionRepository) ReserveCaseCheckout(ctx context.Context, organizationID, installationID, gameServerID int64, tier, customerID string) (*CaseCheckoutReservation, error) {
	if organizationID <= 0 || installationID <= 0 || gameServerID <= 0 || strings.TrimSpace(customerID) == "" {
		return nil, ErrCaseCheckoutConflict
	}
	const q = `
INSERT INTO case_addon_subscriptions(
    organization_id,installation_id,game_server_id,tier,status,provider,provider_customer_id,checkout_reserved_at)
SELECT i.organization_id,i.id,gs.id,$4,'PENDING','stripe',$5,NOW()
FROM installations i
JOIN discord_guild_connections gc ON gc.id=i.discord_guild_connection_id AND gc.organization_id=i.organization_id
JOIN game_servers gs ON gs.id=i.game_server_id AND gs.guild_id=gc.guild_id AND gs.organization_id=i.organization_id
WHERE i.organization_id=$1 AND i.id=$2 AND gs.id=$3 AND i.status IN ('READY','CONFIGURING')
ON CONFLICT DO NOTHING RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx,q,organizationID,installationID,gameServerID,tier,customerID).Scan(&id)
	if err != nil && !errors.Is(err,pgx.ErrNoRows) {
		return nil, fmt.Errorf("reserve case checkout: %w",err)
	}
	const get = `SELECT id,organization_id,installation_id,game_server_id,tier,
COALESCE(provider_customer_id,''),COALESCE(checkout_session_id,''),COALESCE(checkout_url,''),
status,COALESCE(provider_subscription_id,''),checkout_attempt
FROM case_addon_subscriptions WHERE organization_id=$1 AND installation_id=$2`
	var row CaseCheckoutReservation
	var status,subscriptionID string
	err = r.pool.QueryRow(ctx,get,organizationID,installationID).Scan(&row.ID,&row.OrganizationID,&row.InstallationID,&row.GameServerID,
		&row.Tier,&row.ProviderCustomerID,&row.SessionID,&row.CheckoutURL,&status,&subscriptionID,&row.Attempt)
	if errors.Is(err,pgx.ErrNoRows) {return nil,ErrCaseCheckoutConflict}
	if err!=nil {return nil,fmt.Errorf("load case checkout reservation: %w",err)}
	if row.GameServerID!=gameServerID || row.Tier!=tier || row.ProviderCustomerID!=customerID || status!="PENDING" || subscriptionID!="" {
		return nil,ErrCaseCheckoutConflict
	}
	// A stale session must not be replaced while it might have completed at
	// Stripe; an operator can reconcile/expire it in the next lifecycle phase.
	return &row,nil
}

// StoreCaseCheckout records the one Stripe Session returned for a reservation.
// A retry with the same Stripe idempotency key cannot replace a different
// session. The URL remains backend-only.
func (r *CaseAddonSubscriptionRepository) StoreCaseCheckout(ctx context.Context, id int64, sessionID, checkoutURL string) error {
	if id<=0 || sessionID=="" || checkoutURL=="" {return ErrCaseCheckoutConflict}
	tag,err:=r.pool.Exec(ctx,`
UPDATE case_addon_subscriptions
SET checkout_session_id=$2,checkout_url=$3,updated_at=NOW()
WHERE id=$1 AND status='PENDING' AND provider_subscription_id IS NULL
AND (checkout_session_id IS NULL OR checkout_session_id=$2)`,id,sessionID,checkoutURL)
	if err!=nil {return fmt.Errorf("store case checkout: %w",err)}
	if tag.RowsAffected()!=1 {return ErrCaseCheckoutConflict}
	return nil
}

// GetByCaseSubscriptionID is used to classify invoice webhook events before
// they can reach the legacy one-row-per-organization base reconciler.
func (r *CaseAddonSubscriptionRepository) GetByCaseSubscriptionID(ctx context.Context, subscriptionID string) (*CaseAddonSubscription, error) {
	if subscriptionID=="" {return nil,nil}
	const q=`SELECT `+caseAddonColumns+` FROM case_addon_subscriptions c
JOIN installations i ON i.id=c.installation_id AND i.organization_id=c.organization_id
WHERE c.provider='stripe' AND c.provider_subscription_id=$1`
	row,err:=scanCaseAddon(r.pool.QueryRow(ctx,q,subscriptionID))
	if errors.Is(err,pgx.ErrNoRows){return nil,nil}
	if err!=nil{return nil,fmt.Errorf("lookup case subscription: %w",err)}
	return &row,nil
}

// ApplyCaseWebhook locks the exact reserved add-on and records the processed
// event in the SAME PostgreSQL transaction as the subscription update. Failed
// processing leaves no dedupe marker, allowing Stripe's retry to recover.
func (r *CaseAddonSubscriptionRepository) ApplyCaseWebhook(ctx context.Context, in CaseWebhookState) error {
	if in.EventID=="" || in.EventType=="" || in.AddonID<=0 || in.SubscriptionID=="" ||
		in.CustomerID=="" || in.PriceID=="" || in.OrganizationID<=0 || in.InstallationID<=0 || in.GameServerID<=0 {
		return ErrCaseWebhookMismatch
	}
	tx,err:=r.pool.Begin(ctx)
	if err!=nil{return fmt.Errorf("begin case webhook: %w",err)}
	defer tx.Rollback(ctx)
	var orgID,installationID,serverID int64
	var tier,customer,priorSub,sessionID string
	err=tx.QueryRow(ctx,`SELECT organization_id,installation_id,game_server_id,tier,
COALESCE(provider_customer_id,''),COALESCE(provider_subscription_id,''),COALESCE(checkout_session_id,'')
FROM case_addon_subscriptions WHERE id=$1 FOR UPDATE`,in.AddonID).
Scan(&orgID,&installationID,&serverID,&tier,&customer,&priorSub,&sessionID)
	if errors.Is(err,pgx.ErrNoRows){return ErrCaseWebhookMismatch}
	if err!=nil{return fmt.Errorf("lock case addon: %w",err)}
	if orgID!=in.OrganizationID || installationID!=in.InstallationID ||
		serverID!=in.GameServerID || tier!=in.Tier || customer!=in.CustomerID ||
		sessionID=="" || (in.CheckoutSessionID!="" && in.CheckoutSessionID!=sessionID) ||
		(priorSub!="" && priorSub!=in.SubscriptionID) {
		return ErrCaseWebhookMismatch
	}
	if (in.Status=="ACTIVE" || in.Status=="TRIAL") && (in.CurrentPeriodEnd==nil ||
		(in.Status=="TRIAL" && in.TrialEnd==nil)) {
		return ErrCaseWebhookMismatch
	}
	if in.FounderTrialOffer {
		if in.Status!="TRIAL" || in.Tier!="CASE_PRO" ||
			in.TrialStart==nil || in.TrialEnd==nil ||
			!in.TrialEnd.After(*in.TrialStart) ||
			in.TrialEnd.Sub(*in.TrialStart)>7*24*time.Hour {
			return ErrCaseWebhookMismatch
		}
	}
	if in.Status!="ACTIVE" && in.Status!="TRIAL" && in.Status!="PAST_DUE" &&
		in.Status!="CANCELED" && in.Status!="SUSPENDED" {return ErrCaseWebhookMismatch}
	if in.FounderTrialOffer {
		// The one-time grant and event state live in the same transaction.
		// A second subscription for the same organization/server cannot
		// receive another founder trial, even after cancellation/reinstall.
		var grantedID int64
		err=tx.QueryRow(ctx,`INSERT INTO case_addon_trial_grants(
			organization_id,game_server_id,installation_id,addon_id,
			provider_subscription_id,tier,trial_started_at,trial_ends_at)
			VALUES($1,$2,$3,$4,$5,'CASE_PRO',$6,$7)
			ON CONFLICT DO NOTHING RETURNING addon_id`,
			in.OrganizationID,in.GameServerID,in.InstallationID,in.AddonID,
			in.SubscriptionID,in.TrialStart,in.TrialEnd).Scan(&grantedID)
		if errors.Is(err,pgx.ErrNoRows) {
			err=tx.QueryRow(ctx,`SELECT addon_id FROM case_addon_trial_grants
				WHERE organization_id=$1 AND game_server_id=$2
				AND installation_id=$3 AND addon_id=$4
				AND provider_subscription_id=$5 AND trial_started_at=$6 AND trial_ends_at=$7`,
				in.OrganizationID,in.GameServerID,in.InstallationID,in.AddonID,
				in.SubscriptionID,in.TrialStart,in.TrialEnd).Scan(&grantedID)
			if errors.Is(err,pgx.ErrNoRows) {return ErrCaseTrialAlreadyUsed}
		}
		if err!=nil{return fmt.Errorf("grant founder trial: %w",err)}
		if grantedID!=in.AddonID{return ErrCaseTrialAlreadyUsed}
		_,err=tx.Exec(ctx,`UPDATE case_addon_subscriptions
			SET trial_started_at=$2,trial_ends_at=$3 WHERE id=$1`,
			in.AddonID,in.TrialStart,in.TrialEnd)
		if err!=nil{return fmt.Errorf("persist founder trial period: %w",err)}
	}
	var marker int64
	err=tx.QueryRow(ctx,`INSERT INTO case_addon_webhook_events(provider,event_id,event_type,addon_id)
VALUES('stripe',$1,$2,$3) ON CONFLICT DO NOTHING RETURNING addon_id`,
		in.EventID,in.EventType,in.AddonID).Scan(&marker)
	if errors.Is(err,pgx.ErrNoRows) {
		// A duplicate may be acknowledged only after the first attempt's
		// transaction committed successfully.
		return nil
	}
	if err!=nil{return fmt.Errorf("record case event: %w",err)}
	if marker!=in.AddonID{return ErrCaseWebhookMismatch}
	_,err=tx.Exec(ctx,`UPDATE case_addon_subscriptions
SET provider='stripe',provider_customer_id=$2,provider_subscription_id=$3,
provider_price_id=$4,tier=$5,status=$6,current_period_start=$7,current_period_end=$8,
trial_ends_at=$9,cancel_at_period_end=$10,paid_through=COALESCE(GREATEST(paid_through,$11),paid_through,$11),updated_at=NOW()
WHERE id=$1`,in.AddonID,in.CustomerID,in.SubscriptionID,in.PriceID,in.Tier,in.Status,
		in.CurrentPeriodStart,in.CurrentPeriodEnd,in.TrialEnd,in.CancelAtPeriodEnd,in.PaidThrough)
	if err!=nil{return fmt.Errorf("apply case subscription: %w",err)}
	if err=tx.Commit(ctx);err!=nil{return fmt.Errorf("commit case webhook: %w",err)}
	return nil
}

// Compile-time proof that this repository's storage remains Postgres-backed.
var _ = (*pgxpool.Pool)(nil)

// SaveCaseCancelFlag is only a scoped materialized view of Stripe's confirmed
// cancellation state. No base billing rows or entitlement periods are altered.
func (r *CaseAddonSubscriptionRepository) SaveCaseCancelFlag(ctx context.Context, orgID, installationID int64, subID string, cancel bool) error {
	if orgID<=0 || installationID<=0 || subID=="" {return ErrCaseCheckoutConflict}
	tag,err:=r.pool.Exec(ctx,`UPDATE case_addon_subscriptions
SET cancel_at_period_end=$4,updated_at=NOW()
WHERE organization_id=$1 AND installation_id=$2 AND provider_subscription_id=$3
AND provider='stripe' AND status IN ('ACTIVE','TRIAL')`,orgID,installationID,subID,cancel)
	if err!=nil{return fmt.Errorf("save case cancellation: %w",err)}
	if tag.RowsAffected()!=1{return ErrCaseCheckoutConflict}
	return nil
}

// GetPendingCaseCheckout returns only a reserved server's CURRENT pending
// session. It does not create any subscription or refresh a pending purchase.
func (r *CaseAddonSubscriptionRepository) GetPendingCaseCheckout(ctx context.Context, orgID, installationID int64) (*CaseCheckoutReservation,error) {
	if orgID<=0 || installationID<=0 {return nil,ErrCaseCheckoutConflict}
	const q=`SELECT c.id,c.organization_id,c.installation_id,c.game_server_id,c.tier,
	COALESCE(c.provider_customer_id,''),COALESCE(c.checkout_session_id,''),
	COALESCE(c.checkout_url,''),c.checkout_attempt
	FROM case_addon_subscriptions c
	JOIN installations i ON i.id=c.installation_id AND i.organization_id=c.organization_id
	WHERE c.organization_id=$1 AND c.installation_id=$2 AND c.status='PENDING'
	AND c.provider='stripe' AND c.provider_subscription_id IS NULL`
	var row CaseCheckoutReservation
	err:=r.pool.QueryRow(ctx,q,orgID,installationID).Scan(&row.ID,&row.OrganizationID,
		&row.InstallationID,&row.GameServerID,&row.Tier,&row.ProviderCustomerID,
		&row.SessionID,&row.CheckoutURL,&row.Attempt)
	if errors.Is(err,pgx.ErrNoRows){return nil,ErrCaseCheckoutConflict}
	if err!=nil{return nil,fmt.Errorf("read pending case checkout: %w",err)}
	if row.SessionID=="" || row.ProviderCustomerID=="" || row.Attempt<=0 {return nil,ErrCaseCheckoutConflict}
	return &row,nil
}

// ResetExpiredCaseCheckout requires the EXACT session and attempt observed
// before Stripe confirmed it expired. A completed webhook wins the race: it
// sets provider_subscription_id and this compare-and-swap then fails.
func (r *CaseAddonSubscriptionRepository) ResetExpiredCaseCheckout(ctx context.Context, orgID, installationID, addonID, attempt int64, sessionID string) error {
	if orgID<=0 || installationID<=0 || addonID<=0 || attempt<=0 || sessionID=="" {return ErrCaseCheckoutConflict}
	tag,err:=r.pool.Exec(ctx,`UPDATE case_addon_subscriptions
	SET checkout_session_id=NULL,checkout_url=NULL,checkout_attempt=checkout_attempt+1,
	checkout_reserved_at=NOW(),updated_at=NOW()
	WHERE id=$1 AND organization_id=$2 AND installation_id=$3
	AND checkout_attempt=$4 AND checkout_session_id=$5
	AND status='PENDING' AND provider='stripe' AND provider_subscription_id IS NULL`,
	addonID,orgID,installationID,attempt,sessionID)
	if err!=nil{return fmt.Errorf("reset expired case checkout: %w",err)}
	if tag.RowsAffected()!=1{return ErrCaseCheckoutConflict}
	return nil
}
