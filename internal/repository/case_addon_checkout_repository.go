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
)

type CaseCheckoutReservation struct {
	ID int64
	OrganizationID, InstallationID, GameServerID int64
	Tier, ProviderCustomerID, SessionID, CheckoutURL string
}

type CaseWebhookState struct {
	EventID, EventType string
	AddonID, OrganizationID, InstallationID, GameServerID int64
	Tier, CustomerID, SubscriptionID, PriceID, Status string
	CheckoutSessionID string
	CurrentPeriodStart, CurrentPeriodEnd, TrialEnd, PaidThrough *time.Time
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
status,COALESCE(provider_subscription_id,'')
FROM case_addon_subscriptions WHERE organization_id=$1 AND installation_id=$2`
	var row CaseCheckoutReservation
	var status,subscriptionID string
	err = r.pool.QueryRow(ctx,get,organizationID,installationID).Scan(&row.ID,&row.OrganizationID,&row.InstallationID,&row.GameServerID,
		&row.Tier,&row.ProviderCustomerID,&row.SessionID,&row.CheckoutURL,&status,&subscriptionID)
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
	if in.Status!="ACTIVE" && in.Status!="TRIAL" && in.Status!="PAST_DUE" &&
		in.Status!="CANCELED" && in.Status!="SUSPENDED" {return ErrCaseWebhookMismatch}
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
