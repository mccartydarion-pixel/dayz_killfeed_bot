package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// classifyCaseEvent runs AFTER Stripe signature validation but BEFORE legacy
// base-billing webhook deduplication or reconciliation. This is the critical
// guard against writing a server add-on into subscriptions.plan.
func (s *Service) classifyCaseEvent(ctx context.Context, e ParsedEvent) (bool,error) {
	switch e.Type {
	case EventCheckoutCompleted:
		if e.Session==nil{return false,nil}
		if caseTagged(e.Session.Metadata) {return true,nil}
		// If Checkout metadata is missing, inspect its actual Stripe subscription
		// before allowing the base handler to touch subscriptions.plan.
		if e.Session.Subscription=="" || s.provider==nil{return false,nil}
		st,err:=s.provider.GetSubscription(ctx,string(e.Session.Subscription))
		if err!=nil{return false,err}
		return caseTagged(st.Metadata) || s.caseTierForPrice(st.PriceID)!="",nil
	case EventSubscriptionCreated,EventSubscriptionUpdated,EventSubscriptionDeleted:
		if e.Sub==nil{return false,nil}
		if caseTagged(e.Sub.Metadata) {return true,nil}
		for _,item:=range e.Sub.Items.Data {
			if s.caseTierForPrice(item.Price.ID)!="" {return true,nil}
		}
		if s.caseStore!=nil {
			row,err:=s.caseStore.GetByCaseSubscriptionID(ctx,e.Sub.ID)
			return row!=nil,err
		}
		return false,nil
	case EventInvoicePaid,EventInvoicePaymentFailed:
		if e.Invoice==nil || e.Invoice.Subscription=="" {return false,nil}
		subID:=string(e.Invoice.Subscription)
		if s.caseStore!=nil {
			row,err:=s.caseStore.GetByCaseSubscriptionID(ctx,subID)
			if err!=nil{return false,err}
			if row!=nil{return true,nil}
		}
		// A known base subscription can take its existing fast path.
		base,err:=s.store.GetByProviderSubscriptionID(ctx,repository.ProviderStripe,subID)
		if err!=nil{return false,err}
		if base!=nil{return false,nil}
		// First invoice may precede checkout.session.completed; its parent
		// subscription still carries the server-authored product-kind metadata.
		if s.provider==nil{return false,ErrProviderNotConfigured}
		st,err:=s.provider.GetSubscription(ctx,subID)
		if err!=nil{return false,err}
		return caseTagged(st.Metadata) || s.caseTierForPrice(st.PriceID)!="",nil
	}
	return false,nil
}

func caseTagged(meta map[string]string) bool {
	return meta["champion_product_kind"]==caseProductKind ||
		meta["champion_case_addon_id"]!="" ||
		meta["champion_case_tier"]!=""
}

func (s *Service) caseTierForPrice(priceID string) casebilling.Tier {
	for tier,id:=range s.casePrices {if id!="" && id==priceID{return tier}}
	return ""
}

func parseCaseInt(meta map[string]string,key string) (int64,error) {
	raw:=strings.TrimSpace(meta[key])
	id,err:=strconv.ParseInt(raw,10,64)
	if err!=nil || id<=0{return 0,repository.ErrCaseWebhookMismatch}
	return id,nil
}

// applyCaseEvent verifies the pending checkout's exact billing identity, then
// updates only case_addon_subscriptions in one transaction with its event marker.
func (s *Service) applyCaseEvent(ctx context.Context,e ParsedEvent) error {
	if s.caseStore==nil || s.provider==nil{return ErrProviderNotConfigured}
	var subID,customerID,sessionID string
	var eventMeta map[string]string
	switch e.Type {
	case EventCheckoutCompleted:
		if e.Session==nil || e.Session.Mode!="subscription" || e.Session.Subscription=="" {
			return repository.ErrCaseWebhookMismatch
		}
		subID=string(e.Session.Subscription)
		customerID=string(e.Session.Customer)
		sessionID=e.Session.ID
		eventMeta=e.Session.Metadata
	case EventSubscriptionCreated,EventSubscriptionUpdated,EventSubscriptionDeleted:
		if e.Sub==nil || e.Sub.ID=="" {return repository.ErrCaseWebhookMismatch}
		subID=e.Sub.ID
		customerID=string(e.Sub.Customer)
		eventMeta=e.Sub.Metadata
	case EventInvoicePaid,EventInvoicePaymentFailed:
		if e.Invoice==nil || e.Invoice.Subscription=="" {return repository.ErrCaseWebhookMismatch}
		subID=string(e.Invoice.Subscription)
		customerID=string(e.Invoice.Customer)
	default:
		return nil
	}
	var st *SubscriptionState
	var err error
	if e.Type==EventSubscriptionDeleted {
		// Stripe may return 404 after deletion; signed deletion payload is
		// authoritative for cancellation only, never for granting access.
		st=e.Sub.state()
		st.StripeStatus="canceled"
	} else {
		st,err=s.provider.GetSubscription(ctx,subID)
		if err!=nil{return fmt.Errorf("retrieve case subscription: %w",err)}
	}
	if st==nil || st.SubscriptionID!=subID || st.CustomerID=="" ||
		(customerID!="" && customerID!=st.CustomerID) {
		return repository.ErrCaseWebhookMismatch
	}
	meta:=st.Metadata
	if len(meta)==0 {
		meta=eventMeta
	}
	if !caseTagged(meta) || meta["champion_product_kind"]!=caseProductKind {
		// Known add-on from an invoice can recover from missing metadata
		// without trusting the customer's base subscription identity.
		known,err:=s.caseStore.GetByCaseSubscriptionID(ctx,subID)
		if err!=nil{return err}
		if known==nil || known.ProviderCustomerID!=st.CustomerID {return repository.ErrCaseWebhookMismatch}
		meta=map[string]string{
			"champion_product_kind":caseProductKind,
			"champion_case_addon_id":strconv.FormatInt(known.ID,10),
			"champion_organization_id":strconv.FormatInt(known.OrganizationID,10),
			"champion_installation_id":strconv.FormatInt(known.InstallationID,10),
			"champion_game_server_id":strconv.FormatInt(known.GameServerID,10),
			"champion_case_tier":known.Tier,
		}
	}
	if caseTagged(eventMeta) {
		for _,k:=range []string{"champion_product_kind","champion_case_addon_id","champion_organization_id",
			"champion_installation_id","champion_game_server_id","champion_case_tier"} {
			if meta[k]!=eventMeta[k] {return repository.ErrCaseWebhookMismatch}
		}
	}
	addonID,err:=parseCaseInt(meta,"champion_case_addon_id");if err!=nil{return err}
	orgID,err:=parseCaseInt(meta,"champion_organization_id");if err!=nil{return err}
	installationID,err:=parseCaseInt(meta,"champion_installation_id");if err!=nil{return err}
	serverID,err:=parseCaseInt(meta,"champion_game_server_id");if err!=nil{return err}
	tier:=casebilling.Tier(meta["champion_case_tier"])
	if _,ok:=casebilling.Lookup(string(tier)); !ok ||
		s.caseTierForPrice(st.PriceID)!=tier {return repository.ErrCaseWebhookMismatch}
	founderTag:=meta["champion_case_trial_offer"]=="founder_pro_7d"
	if meta["champion_case_trial_offer"]!="" && !founderTag {
		return repository.ErrCaseWebhookMismatch
	}
	status:=MapStatus(st.StripeStatus)
	// The marker remains on paid subscriptions after a trial finishes.
	// Never attempt to grant the seven-day offer on a paid renewal event.
	founderOffer:=founderTag && status==repository.SubscriptionTrial
	if e.Type==EventSubscriptionDeleted {status=repository.SubscriptionCanceled}
	// We deliberately fail closed on a failed invoice even if a payment
	// method left Stripe reporting ACTIVE. A later paid event reconciles it.
	if e.Type==EventInvoicePaymentFailed && status!=repository.SubscriptionCanceled {
		status=repository.SubscriptionPastDue
	}
	var paidThrough *time.Time
	if e.Type==EventInvoicePaid {
		// Only Stripe's signed invoice.paid with a real subscription line
		// proves paid coverage. Checkout/ACTIVE alone never does.
		_,end:=e.Invoice.period()
		if end.IsZero() {return repository.ErrCaseWebhookMismatch}
		paidThrough=&end
	}
	err=s.caseStore.ApplyCaseWebhook(ctx,repository.CaseWebhookState{
		EventID:e.ID,EventType:e.Type,AddonID:addonID,OrganizationID:orgID,InstallationID:installationID,
		GameServerID:serverID,Tier:string(tier),CustomerID:st.CustomerID,
		SubscriptionID:subID,PriceID:st.PriceID,Status:status,
		CheckoutSessionID:sessionID,CurrentPeriodStart:zeroToNil(st.CurrentPeriodStart),
		CurrentPeriodEnd:zeroToNil(st.CurrentPeriodEnd),TrialStart:st.TrialStart,TrialEnd:st.TrialEnd, PaidThrough:paidThrough,
		FounderTrialOffer:founderOffer,
		CancelAtPeriodEnd:st.CancelAtPeriodEnd,
	})
	if err!=nil{return fmt.Errorf("apply isolated case webhook: %w",err)}
	slog.Info("component=case_billing","event","case_subscription_reconciled",
		"organization_id",orgID,"installation_id",installationID,"tier",tier,"status",status,"stripe_event_id",e.ID)
	return nil
}

// ClassifyCaseEventForTest allows testing classification without a Stripe API
// mutation or a live billing operation.
func (s *Service) ClassifyCaseEventForTest(ctx context.Context,e ParsedEvent)(bool,error){
	return s.classifyCaseEvent(ctx,e)
}

