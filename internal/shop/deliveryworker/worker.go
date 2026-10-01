// Package deliveryworker is the Champion Shop automatic delivery worker
// (docs/SHOP_DELIVERY_WORKER_DESIGN.md). It does by itself what the canary did by hand: it puts a
// bought item into the Champion spawner file, waits for a server restart, removes it again and asks
// the buyer whether it arrived.
//
// Rules it never breaks:
//   - it writes one file only, custom/champion_shop_delivery.json, at most once per pass, and decides
//     every write's outcome by an independent read-back;
//   - every write is journaled in the database before it is sent; an unfinished or uncertain write
//     stops all further writes to that server until a person resumes it;
//   - it never restarts a server, never edits cfggameplay.json, never stages an order again after an
//     attempt that might have spawned, never refunds, and never marks an order fulfilled without the
//     buyer's answer (the database enforces the last one);
//   - when something is unclear it pauses the installation and opens a ticket for staff.
//
// The package has no HTTP client and no SQL: it works through the interfaces below, so the whole
// state machine runs in tests against fakes and against the Nitrado fixture.
package deliveryworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/missionwrite"
	nd "github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

const (
	// StagingLeadTime: a restart that began less than this after the staged file was written may have
	// copied the mission before the write landed. Whether the item spawned is then unknown.
	StagingLeadTime = 3 * time.Minute
	// LogEvidenceWait is how long after the unstage the worker waits for the server's own log to
	// show that the boot finished starting, before it hands the order to a person.
	LogEvidenceWait = 30 * time.Minute
	// DefaultMaxStaged is the number of orders in the file at once until batching is approved.
	DefaultMaxStaged = 1
)

// Server is one installation's Nitrado service.
type Server interface {
	Inspect(ctx context.Context) (missionwrite.ArtifactState, error)
	Write(ctx context.Context, st missionwrite.ArtifactState, payload []byte) (missionwrite.ArtifactWrite, error)
}

// Ledger is the attempt ledger (repository.ShopAttemptRepository).
type Ledger interface {
	ListOpen(ctx context.Context, org, inst int64) ([]repository.ShopAttempt, error)
	Create(ctx context.Context, in repository.ShopAttemptCreate, actor string) (repository.ShopAttempt, error)
	Transition(ctx context.Context, org, inst int64, attemptID, from, to, actor string, ev repository.ShopAttemptEvidence) (repository.ShopAttempt, error)
	NextAttemptNumber(ctx context.Context, org, inst, deliveryID int64) (int, error)
	FulfillAttemptByBuyer(ctx context.Context, org, inst int64, attemptID, answer string, answeredAt time.Time, actor string) (*repository.ShopPurchase, error)
}

// Store is the worker's own storage (repository.ShopAutoDeliveryRepository).
type Store interface {
	Pause(ctx context.Context, org, inst int64, reason string) error
	AcceptConfiguration(ctx context.Context, org, inst int64, configSHA256, missionPath string) error
	BeginWrite(ctx context.Context, w repository.ShopDeliveryWrite) (repository.ShopDeliveryWrite, error)
	FinishWrite(ctx context.Context, id int64, outcome, afterSHA256, detail string) error
	UnresolvedWrite(ctx context.Context, inst int64) (*repository.ShopDeliveryWrite, error)
	Candidates(ctx context.Context, org, inst, serverID int64, limit int) ([]repository.ShopAutoCandidate, error)
	BootLog(ctx context.Context, serverID int64, from, to time.Time) (repository.ShopBootLog, error)
}

// Deliveries reads Shop deliveries (repository.ShopRepository).
type Deliveries interface {
	GetDelivery(ctx context.Context, org, inst, id, playerID int64) (*repository.ShopDelivery, error)
}

// Buyers is the buyer confirmation and ticket store (repository.ShopConfirmationRepository).
type Buyers interface {
	OpenForDelivered(ctx context.Context, org, inst, purchaseID int64) error
	GetConfirmation(ctx context.Context, org, inst, purchaseID, playerID int64) (repository.ShopOrderConfirmation, error)
	OpenSystemTicket(ctx context.Context, org, inst, purchaseID int64, reason string) (repository.ShopOrderTicket, error)
}

// Config tunes a worker.
type Config struct {
	// Name identifies this worker in the ledger and the journal ("worker:<name>").
	Name string
	// ReportOnly makes every pass read-only: it logs what it would do and writes nothing, to the
	// server or to the database.
	ReportOnly bool
	// MaxStaged is the most orders in the Champion file at once (0 = DefaultMaxStaged).
	MaxStaged int
}

// Worker runs passes. It holds no state between passes: everything it knows is in the database and
// on the server, so a restarted worker continues where the last one stopped.
type Worker struct {
	cfg        Config
	ledger     Ledger
	store      Store
	deliveries Deliveries
	buyers     Buyers
	now        func() time.Time
	// OnTicket, when set, is told about a ticket the worker opened (the Discord channel is created
	// from it). It must not block.
	OnTicket func(ticketID int64)
}

func New(cfg Config, ledger Ledger, store Store, deliveries Deliveries, buyers Buyers) *Worker {
	if cfg.MaxStaged <= 0 {
		cfg.MaxStaged = DefaultMaxStaged
	}
	if cfg.MaxStaged > nd.MaxStagedObjects/nd.MaxUnitsPerOrder {
		cfg.MaxStaged = nd.MaxStagedObjects / nd.MaxUnitsPerOrder
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "default"
	}
	return &Worker{cfg: cfg, ledger: ledger, store: store, deliveries: deliveries, buyers: buyers, now: time.Now}
}

// SetClock replaces the clock (tests).
func (w *Worker) SetClock(now func() time.Time) { w.now = now }

func (w *Worker) actor() string { return "worker:" + w.cfg.Name }

// Report is what one pass did, for logs and tests.
type Report struct {
	InstallationID int64
	Inspected      bool
	Paused         string   // the reason, when this pass paused the installation
	Skipped        string   // why the pass did nothing on the server
	Staged         []string // attempt ids written into the file
	Unstaged       []string // attempt ids removed from the file and now awaiting the buyer
	Failed         []string // attempt ids sent to review
	Fulfilled      []string
	Delivered      []string // attempt ids whose buyer was asked
	Abandoned      []string
	WouldStage     []int64 // report-only: delivery ids that qualify
	CurrentBoot    string
	WriteOutcome   string
}

var errPaused = errors.New("deliveryworker: installation paused")

// pass carries one pass's state.
type pass struct {
	w    *Worker
	ctx  context.Context
	inst repository.ShopAutoInstallation
	srv  Server
	rep  *Report
}

func (p *pass) pause(reason string) error {
	p.rep.Paused = reason
	slog.Error("component=shop_delivery_worker", "event", "paused", "installation_id", p.inst.InstallationID, "reason", reason,
		"action", "a person must check the server and resume automatic delivery")
	if p.w.cfg.ReportOnly {
		return errPaused
	}
	if err := p.w.store.Pause(p.ctx, p.inst.OrganizationID, p.inst.InstallationID, reason); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	return errPaused
}

// Pass works one installation once. The caller holds the installation's lease.
func (w *Worker) Pass(ctx context.Context, inst repository.ShopAutoInstallation, srv Server) (Report, error) {
	rep := Report{InstallationID: inst.InstallationID}
	p := &pass{w: w, ctx: ctx, inst: inst, srv: srv, rep: &rep}
	err := p.run()
	if errors.Is(err, errPaused) {
		err = nil
	}
	return rep, err
}

func isBuyerMode(a repository.ShopAttempt) bool {
	return a.FulfilmentMode == repository.ShopFulfilmentBuyer
}

// inFile reports whether an attempt in this state is expected to be in the Champion file.
func inFile(state string) bool {
	switch state {
	case repository.AttemptFileStaged, repository.AttemptAwaitingRestart, repository.AttemptRestartObserved, repository.AttemptUnstageRequired:
		return true
	}
	return false
}

func entriesOf(attempts []repository.ShopAttempt) []nd.SpawnerObject {
	var out []nd.SpawnerObject
	for _, a := range attempts {
		out = append(out, nd.AttemptEntries(a.AttemptID, a.ClassName, a.Quantity, [3]float64{a.PosX, a.PosY, a.PosZ})...)
	}
	return out
}

// render is the exact file for a set of attempts (the empty file for none).
func render(attempts []repository.ShopAttempt) []byte {
	objs := entriesOf(attempts)
	if objs == nil {
		objs = []nd.SpawnerObject{}
	}
	return nd.SpawnerFile{Objects: objs}.Render()
}

func ids(attempts []repository.ShopAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.AttemptID)
	}
	sort.Strings(out)
	return out
}

func ptr[T any](v T) *T { return &v }

func (p *pass) run() error {
	w, inst := p.w, p.inst
	open, err := w.ledger.ListOpen(p.ctx, inst.OrganizationID, inst.InstallationID)
	if err != nil {
		return fmt.Errorf("list open attempts: %w", err)
	}
	var planned, prepared, staged, verifying []repository.ShopAttempt
	for _, a := range open {
		if !isBuyerMode(a) {
			// A manual (canary) attempt is in progress on this server: the file is the operator's.
			p.rep.Skipped = "a manual delivery attempt is open on this server"
			return nil
		}
		switch {
		case a.State == repository.AttemptPlanCreated:
			planned = append(planned, a)
		case a.State == repository.AttemptFilePrepared:
			prepared = append(prepared, a)
		case inFile(a.State):
			staged = append(staged, a)
		case a.State == repository.AttemptVerificationRequired:
			verifying = append(verifying, a)
		}
	}
	if !w.cfg.ReportOnly {
		if err := p.askBuyers(verifying); err != nil {
			return err
		}
	}

	unresolved, err := w.store.UnresolvedWrite(p.ctx, inst.InstallationID)
	if err != nil {
		return fmt.Errorf("read the write journal: %w", err)
	}
	if unresolved != nil && unresolved.Outcome == repository.ShopWriteUncertain {
		return p.pause("an earlier write to the Champion file has an uncertain outcome")
	}
	free := w.cfg.MaxStaged - len(staged) - len(prepared)
	var candidates []repository.ShopAutoCandidate
	if free > 0 && unresolved == nil {
		if candidates, err = w.store.Candidates(p.ctx, inst.OrganizationID, inst.InstallationID, inst.GameServerID, free); err != nil {
			return fmt.Errorf("list deliverable orders: %w", err)
		}
	}
	if unresolved == nil && len(planned)+len(prepared)+len(staged)+len(candidates) == 0 && !w.cfg.ReportOnly {
		p.rep.Skipped = "nothing to do"
		return nil
	}

	st, err := p.srv.Inspect(p.ctx)
	switch {
	case errors.Is(err, missionwrite.ErrArtifactMissing):
		return p.pause("the Champion spawner file is missing on the server")
	case errors.Is(err, missionwrite.ErrArtifactNotReferenced):
		return p.pause("cfggameplay.json no longer references the Champion spawner file")
	case errors.Is(err, missionwrite.ErrBinding):
		return p.pause("the Nitrado service is not the one bound to this installation")
	case err != nil:
		// Nitrado unreachable or a listing failed: reading again later is always safe.
		p.rep.Skipped = "the server could not be inspected; trying again on the next pass"
		slog.Warn("component=shop_delivery_worker", "event", "inspect_failed", "installation_id", inst.InstallationID, "err", err.Error())
		return nil
	}
	p.rep.Inspected, p.rep.CurrentBoot = true, st.CurrentBoot()

	switch {
	case inst.ConfigSHA256 == "":
		if !w.cfg.ReportOnly {
			if err := w.store.AcceptConfiguration(p.ctx, inst.OrganizationID, inst.InstallationID, st.ConfigSHA256, st.MissionPath); err != nil {
				return fmt.Errorf("accept configuration: %w", err)
			}
		}
	case inst.ConfigSHA256 != st.ConfigSHA256:
		return p.pause("cfggameplay.json changed since automatic delivery was last resumed")
	case inst.MissionPath != st.MissionPath:
		return p.pause("the server's mission folder changed since automatic delivery was last resumed")
	}
	if _, err := nd.ParseSpawnerFile(st.Content); err != nil {
		return p.pause("the Champion spawner file holds content Champion did not write")
	}

	if w.cfg.ReportOnly {
		for _, c := range candidates {
			p.rep.WouldStage = append(p.rep.WouldStage, c.DeliveryID)
		}
		p.rep.Skipped = "report only"
		return nil
	}

	// A write that was journaled but never finished (the worker stopped mid-write).
	if unresolved != nil {
		recovered, err := p.recover(*unresolved, st, &prepared, &staged)
		if err != nil || !recovered {
			return err
		}
	}

	// Attempts that never reached the journal were never written: safe to plan again.
	for _, a := range append(planned, prepared...) {
		if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptAbandoned, w.actor(),
			repository.ShopAttemptEvidence{Note: "the worker stopped before the file was written"}); err != nil {
			return fmt.Errorf("abandon %s: %w", a.AttemptID, err)
		}
		p.rep.Abandoned = append(p.rep.Abandoned, a.AttemptID)
	}

	if missionwrite.SHA256(render(staged)) != st.SHA256 {
		return p.pause("the Champion spawner file does not match the delivery ledger")
	}

	remove, failing, err := p.decide(staged, st)
	if err != nil {
		return err
	}
	if len(remove) > 0 {
		return p.unstage(st, staged, remove, failing)
	}
	if len(candidates) > 0 {
		return p.stage(st, staged, candidates)
	}
	return nil
}

var admTimeRe = regexp.MustCompile(`_(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})\.ADM$`)

// BootStart is the UTC start of the boot an ADM file belongs to: the server-local time in its name,
// shifted by the server clock's offset from UTC. ok is false when the offset is unknown.
func BootStart(admFile string, utcOffsetMinutes *int) (time.Time, bool) {
	m := admTimeRe.FindStringSubmatch(admFile)
	if m == nil || utcOffsetMinutes == nil {
		return time.Time{}, false
	}
	local, err := time.Parse("2006-01-02_15-04-05", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return local.Add(-time.Duration(*utcOffsetMinutes) * time.Minute), true
}

// bootsAfter are the boots newer than the given ADM file, oldest first.
func bootsAfter(boots []string, file string) []string {
	var out []string
	for _, b := range boots {
		if b > file {
			out = append(out, b)
		}
	}
	return out
}

// decide applies the restart rules to every staged attempt. remove are the attempts to take out of
// the file now; failing maps those among them that go to review to the reason.
func (p *pass) decide(staged []repository.ShopAttempt, st missionwrite.ArtifactState) (remove []repository.ShopAttempt, failing map[string]string, err error) {
	w, inst := p.w, p.inst
	failing = map[string]string{}
	for i := range staged {
		a := staged[i]
		if a.StagedBootFile == nil || a.StagedAt == nil {
			return nil, nil, p.pause("a staged attempt has no staging evidence in the ledger")
		}
		newer := bootsAfter(st.Boots, *a.StagedBootFile)
		if len(newer) == 0 {
			if a.State == repository.AttemptFileStaged {
				if staged[i], err = w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptAwaitingRestart, w.actor(),
					repository.ShopAttemptEvidence{Note: "staged; waiting for the next scheduled restart"}); err != nil {
					return nil, nil, fmt.Errorf("advance %s: %w", a.AttemptID, err)
				}
			}
			continue
		}
		remove = append(remove, a)
		start, known := BootStart(newer[0], inst.UTCOffsetMinutes)
		switch {
		case len(newer) >= 2:
			failing[a.AttemptID] = "two restarts happened before the item could be removed from the file: it may have spawned twice"
		case known && start.Before(*a.StagedAt):
			failing[a.AttemptID] = "the restart had already begun when the item was staged: whether it spawned is unknown"
		case known && start.Sub(*a.StagedAt) < StagingLeadTime:
			failing[a.AttemptID] = "the item was staged less than three minutes before the restart: whether it spawned is unknown"
		}
		if _, bad := failing[a.AttemptID]; bad {
			continue
		}
		// One clean restart after staging: record it. The attempt reaches UNSTAGE_REQUIRED.
		observed := p.w.now()
		if known {
			observed = start
		}
		if a.State == repository.AttemptFileStaged {
			if a, err = w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptAwaitingRestart, w.actor(),
				repository.ShopAttemptEvidence{Note: "staged"}); err != nil {
				return nil, nil, fmt.Errorf("advance %s: %w", a.AttemptID, err)
			}
		}
		if a.State == repository.AttemptAwaitingRestart {
			if a, err = w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptRestartObserved, w.actor(),
				repository.ShopAttemptEvidence{RestartBootFile: ptr(newer[0]), RestartObservedAt: &observed, Note: "new boot " + newer[0]}); err != nil {
				return nil, nil, fmt.Errorf("advance %s: %w", a.AttemptID, err)
			}
		}
		if a.State == repository.AttemptRestartObserved {
			if a, err = w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptUnstageRequired, w.actor(),
				repository.ShopAttemptEvidence{Note: "remove the entry before the next restart"}); err != nil {
				return nil, nil, fmt.Errorf("advance %s: %w", a.AttemptID, err)
			}
		}
		staged[i] = a
		remove[len(remove)-1] = a
	}
	return remove, failing, nil
}

// journal journals a write, sends it and returns its verified outcome. The journal row stays STARTED
// until settle is called: the ledger is updated first, so a worker that stops in between leaves a
// STARTED row and the next pass redoes the ledger from the file's read-back (recover). A row is
// never finished while the ledger still disagrees with the file.
func (p *pass) journal(kind string, st missionwrite.ArtifactState, payload []byte, attempts []repository.ShopAttempt) (repository.ShopDeliveryWrite, missionwrite.ArtifactWrite, error) {
	w, inst := p.w, p.inst
	entry, err := w.store.BeginWrite(p.ctx, repository.ShopDeliveryWrite{OrganizationID: inst.OrganizationID, InstallationID: inst.InstallationID, Kind: kind,
		AttemptIDs: ids(attempts), BeforeSHA256: st.SHA256, PayloadSHA256: missionwrite.SHA256(payload), BootFile: st.CurrentBoot(), Worker: w.actor()})
	if err != nil {
		return entry, missionwrite.ArtifactWrite{}, fmt.Errorf("journal the write: %w", err)
	}
	res, werr := p.srv.Write(p.ctx, st, payload)
	if werr != nil && res.Status == "" {
		res.Status = missionwrite.StatusNotWritten
	}
	if res.Detail == "" && werr != nil {
		res.Detail = "refused before any write request"
	}
	p.rep.WriteOutcome = res.Status
	slog.Info("component=shop_delivery_worker", "event", "write", "installation_id", inst.InstallationID, "kind", kind, "outcome", res.Status,
		"attempts", len(attempts), "detail", res.Detail)
	return entry, res, nil
}

// settle finishes the journal row once the ledger reflects the write. ledgerErr is what recording
// the write in the ledger returned: only success (or a pause, which is a recorded decision) lets the
// row be finished.
func (p *pass) settle(id int64, outcome, after, detail string, ledgerErr error) error {
	if ledgerErr != nil && !errors.Is(ledgerErr, errPaused) {
		return ledgerErr
	}
	if err := p.w.store.FinishWrite(p.ctx, id, outcome, after, detail); err != nil {
		return fmt.Errorf("journal the outcome %s: %w", outcome, err)
	}
	return ledgerErr
}

func (p *pass) fail(a repository.ShopAttempt, reason string, ev repository.ShopAttemptEvidence) error {
	w, inst := p.w, p.inst
	ev.FailureReason, ev.Note = &reason, reason
	if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptFailedReview, w.actor(), ev); err != nil {
		return fmt.Errorf("send %s to review: %w", a.AttemptID, err)
	}
	p.rep.Failed = append(p.rep.Failed, a.AttemptID)
	return p.ticket(a, reason)
}

func (p *pass) ticket(a repository.ShopAttempt, reason string) error {
	w, inst := p.w, p.inst
	d, err := w.deliveries.GetDelivery(p.ctx, inst.OrganizationID, inst.InstallationID, a.DeliveryID, 0)
	if err != nil {
		return fmt.Errorf("load delivery %d: %w", a.DeliveryID, err)
	}
	t, err := w.buyers.OpenSystemTicket(p.ctx, inst.OrganizationID, inst.InstallationID, d.PurchaseID, "Automatic delivery stopped for this order: "+reason+".")
	if err != nil {
		return fmt.Errorf("open a ticket for %s: %w", a.AttemptID, err)
	}
	if w.OnTicket != nil {
		w.OnTicket(t.ID)
	}
	return nil
}

// unstage removes the given attempts from the file. Those in failing go to review; the others move
// to VERIFICATION_REQUIRED, where the buyer is asked.
func (p *pass) unstage(st missionwrite.ArtifactState, staged, remove []repository.ShopAttempt, failing map[string]string) error {
	gone := map[string]bool{}
	for _, a := range remove {
		gone[a.AttemptID] = true
	}
	var keep []repository.ShopAttempt
	for _, a := range staged {
		if !gone[a.AttemptID] {
			keep = append(keep, a)
		}
	}
	entry, res, err := p.journal(repository.ShopWriteUnstage, st, render(keep), remove)
	if err != nil {
		return err
	}
	return p.settle(entry.ID, res.Status, res.After, res.Detail, p.afterUnstage(res, remove, failing, p.w.now()))
}

func (p *pass) afterUnstage(res missionwrite.ArtifactWrite, remove []repository.ShopAttempt, failing map[string]string, verifiedAt time.Time) error {
	w, inst := p.w, p.inst
	switch res.Status {
	case missionwrite.StatusUncertain:
		return p.pause("removing delivered items from the Champion file had an uncertain outcome: " + res.Detail)
	case missionwrite.StatusNotWritten:
		return nil // nothing changed; the next pass tries again
	}
	for _, a := range remove {
		reason, bad := failing[a.AttemptID]
		if !bad && res.RestartDuring {
			reason, bad = "a restart happened while the item was being removed from the file: it may have spawned twice", true
		}
		ev := repository.ShopAttemptEvidence{UnstagedSHA256: ptr(res.After), UnstageVerifiedAt: &verifiedAt}
		if bad {
			if err := p.fail(a, reason, ev); err != nil {
				return err
			}
			continue
		}
		ev.Note = "removed from the file and verified"
		if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptVerificationRequired, w.actor(), ev); err != nil {
			return fmt.Errorf("advance %s: %w", a.AttemptID, err)
		}
		p.rep.Unstaged = append(p.rep.Unstaged, a.AttemptID)
	}
	if res.ConfigChanged {
		return p.pause("cfggameplay.json changed while the Champion file was being written")
	}
	return nil
}

// stage plans the candidates, writes them into the file and records the staging.
func (p *pass) stage(st missionwrite.ArtifactState, staged []repository.ShopAttempt, candidates []repository.ShopAutoCandidate) error {
	w, inst := p.w, p.inst
	if st.GameserverStatus != "started" {
		p.rep.Skipped = "the server is not running"
		return nil
	}
	start, known := BootStart(st.CurrentBoot(), inst.UTCOffsetMinutes)
	if !known {
		p.rep.Skipped = "the server clock offset is unknown: restarts could not be timed"
		return nil
	}
	if w.now().Sub(start) < StagingQuietPeriod {
		p.rep.Skipped = "the current boot is too recent to stage safely"
		return nil
	}
	var fresh []repository.ShopAttempt
	for _, c := range candidates {
		a, err := p.plan(c)
		if err != nil {
			slog.Warn("component=shop_delivery_worker", "event", "plan_refused", "installation_id", inst.InstallationID, "delivery_id", c.DeliveryID, "err", err.Error())
			continue
		}
		fresh = append(fresh, a)
	}
	if len(fresh) == 0 {
		return nil
	}
	entry, res, err := p.journal(repository.ShopWriteStage, st, render(append(append([]repository.ShopAttempt{}, staged...), fresh...)), fresh)
	if err != nil {
		return err
	}
	return p.settle(entry.ID, res.Status, res.After, res.Detail, p.afterStage(res, fresh, st.SHA256, entry.StartedAt, st.CurrentBoot()))
}

// StagingQuietPeriod: a boot's ADM file is listed up to nine minutes after the boot starts, so for
// that long a newer boot may be running unseen. Nothing is staged until the current boot is older.
const StagingQuietPeriod = 10 * time.Minute

// plan validates one candidate with the production validator and records it as FILE_PREPARED.
func (p *pass) plan(c repository.ShopAutoCandidate) (repository.ShopAttempt, error) {
	w, inst := p.w, p.inst
	var none repository.ShopAttempt
	d, err := w.deliveries.GetDelivery(p.ctx, inst.OrganizationID, inst.InstallationID, c.DeliveryID, 0)
	if err != nil {
		return none, err
	}
	n, err := w.ledger.NextAttemptNumber(p.ctx, inst.OrganizationID, inst.InstallationID, c.DeliveryID)
	if err != nil {
		return none, err
	}
	alt := c.AltitudeY
	plan, err := nd.NewPlan(nd.PlanInput{OrganizationID: inst.OrganizationID, InstallationID: inst.InstallationID,
		Binding:  nd.Binding{OrganizationID: inst.OrganizationID, InstallationID: inst.InstallationID, GameServerID: inst.GameServerID, NitradoServiceID: inst.NitradoServiceID, MapKey: inst.MapKey},
		Delivery: *d, ClassName: c.ClassName, ApprovedClassNames: []string{c.ClassName}, AltitudeY: &alt, Attempt: n})
	if err != nil {
		return none, err
	}
	pos := plan.Position()
	a, err := w.ledger.Create(p.ctx, repository.ShopAttemptCreate{OrganizationID: inst.OrganizationID, InstallationID: inst.InstallationID, DeliveryID: c.DeliveryID,
		Attempt: n, AttemptID: plan.AttemptID(), Fingerprint: plan.Fingerprint(), ClassName: plan.ClassName(), Quantity: plan.Quantity(),
		PosX: pos[0], PosY: pos[1], PosZ: pos[2], DropSourceFile: c.DropSourceFile, DropSourceOffset: c.DropSourceOffset,
		ArtifactPath: repository.ShopAttemptCustomArtifactPath, FulfilmentMode: repository.ShopFulfilmentBuyer}, w.actor())
	if err != nil {
		return none, err
	}
	return w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptFilePrepared, w.actor(),
		repository.ShopAttemptEvidence{Note: "planned by the delivery worker"})
}

func (p *pass) afterStage(res missionwrite.ArtifactWrite, fresh []repository.ShopAttempt, before string, stagedAt time.Time, boot string) error {
	w, inst := p.w, p.inst
	switch res.Status {
	case missionwrite.StatusUncertain:
		return p.pause("staging a delivery had an uncertain outcome: " + res.Detail)
	case missionwrite.StatusNotWritten:
		for _, a := range fresh {
			if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptAbandoned, w.actor(),
				repository.ShopAttemptEvidence{Note: "the file was verified unchanged: nothing was written"}); err != nil {
				return fmt.Errorf("abandon %s: %w", a.AttemptID, err)
			}
			p.rep.Abandoned = append(p.rep.Abandoned, a.AttemptID)
		}
		return nil
	}
	for _, a := range fresh {
		// staged_at is the moment BEFORE the upload request: any boot that started after it may have
		// read the staged file, which is the conservative reading.
		if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptFileStaged, w.actor(),
			repository.ShopAttemptEvidence{BeforeSHA256: ptr(before), StagedSHA256: ptr(res.After), StagedAt: &stagedAt, StagedBootFile: ptr(boot),
				Note: "staged and verified by read-back"}); err != nil {
			return fmt.Errorf("record staging of %s: %w", a.AttemptID, err)
		}
		p.rep.Staged = append(p.rep.Staged, a.AttemptID)
	}
	if res.ConfigChanged {
		return p.pause("cfggameplay.json changed while the Champion file was being written")
	}
	return nil
}

// recover finishes a write whose outcome was never journaled, from what the file holds now.
// ok is false when the pass must stop (the write did not land and nothing else may be assumed).
func (p *pass) recover(wr repository.ShopDeliveryWrite, st missionwrite.ArtifactState, prepared, staged *[]repository.ShopAttempt) (ok bool, err error) {
	w := p.w
	in := map[string]bool{}
	for _, id := range wr.AttemptIDs {
		in[id] = true
	}
	switch st.SHA256 {
	case wr.PayloadSHA256:
		const detail = "recovered: the file holds the journaled payload"
		res := missionwrite.ArtifactWrite{Status: missionwrite.StatusWrittenVerified, Before: wr.BeforeSHA256, After: st.SHA256}
		if wr.Kind == repository.ShopWriteStage {
			var fresh, rest []repository.ShopAttempt
			for _, a := range *prepared {
				if in[a.AttemptID] {
					fresh = append(fresh, a)
				} else {
					rest = append(rest, a)
				}
			}
			*prepared = rest
			return false, p.settle(wr.ID, repository.ShopWriteWrittenVerified, st.SHA256, detail, p.afterStage(res, fresh, wr.BeforeSHA256, wr.StartedAt, wr.BootFile))
		}
		var remove []repository.ShopAttempt
		for _, a := range *staged {
			if in[a.AttemptID] {
				remove = append(remove, a)
			}
		}
		// The worker stopped between the write and the ledger: what it had decided about each attempt
		// is lost, so every removed attempt in a state that had not reached UNSTAGE_REQUIRED goes to review.
		failing := map[string]string{}
		for _, a := range remove {
			if a.State != repository.AttemptUnstageRequired {
				failing[a.AttemptID] = "the worker stopped while removing this item after an unclear restart"
			}
		}
		return false, p.settle(wr.ID, repository.ShopWriteWrittenVerified, st.SHA256, detail, p.afterUnstage(res, remove, failing, w.now()))
	case wr.BeforeSHA256:
		if err := w.store.FinishWrite(p.ctx, wr.ID, repository.ShopWriteNotWritten, st.SHA256, "recovered: the file is unchanged"); err != nil {
			return false, fmt.Errorf("finish the recovered write: %w", err)
		}
		return true, nil
	}
	if err := w.store.FinishWrite(p.ctx, wr.ID, repository.ShopWriteUncertain, st.SHA256, "recovered: the file is neither the payload nor the previous file"); err != nil {
		return false, fmt.Errorf("finish the recovered write: %w", err)
	}
	return false, p.pause("a write to the Champion file was interrupted and the file is in an unexpected state")
}

// askBuyers moves attempts whose item was removed after one clean restart: first the buyer is asked
// (once the server's own log shows the boot finished without a spawner error), then their answer
// fulfils the order or opens a review.
func (p *pass) askBuyers(verifying []repository.ShopAttempt) error {
	w, inst := p.w, p.inst
	for _, a := range verifying {
		d, err := w.deliveries.GetDelivery(p.ctx, inst.OrganizationID, inst.InstallationID, a.DeliveryID, 0)
		if err != nil {
			return fmt.Errorf("load delivery %d: %w", a.DeliveryID, err)
		}
		c, err := w.buyers.GetConfirmation(p.ctx, inst.OrganizationID, inst.InstallationID, d.PurchaseID, 0)
		if errors.Is(err, repository.ErrShopConfirmationNotFound) {
			if err := p.deliver(a, d.PurchaseID); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read the buyer confirmation of %s: %w", a.AttemptID, err)
		}
		switch c.State {
		case repository.ConfirmationReceived, repository.ConfirmationAutoCompleted:
			answered := w.now()
			if c.RespondedAt != nil {
				answered = *c.RespondedAt
			}
			if a.UnstageVerifiedAt != nil && answered.Before(*a.UnstageVerifiedAt) {
				answered = *a.UnstageVerifiedAt
			}
			if _, err := w.ledger.FulfillAttemptByBuyer(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, c.State, answered, w.actor()); err != nil {
				return fmt.Errorf("fulfil %s: %w", a.AttemptID, err)
			}
			p.rep.Fulfilled = append(p.rep.Fulfilled, a.AttemptID)
		case repository.ConfirmationIssueReported:
			// The buyer's own ticket is already open; the attempt joins it in review.
			reason := "the buyer reported an issue with the delivered order"
			if _, err := w.ledger.Transition(p.ctx, inst.OrganizationID, inst.InstallationID, a.AttemptID, a.State, repository.AttemptFailedReview, w.actor(),
				repository.ShopAttemptEvidence{FailureReason: &reason, Note: reason}); err != nil {
				return fmt.Errorf("send %s to review: %w", a.AttemptID, err)
			}
			p.rep.Failed = append(p.rep.Failed, a.AttemptID)
		}
	}
	return nil
}

// deliver opens the buyer's confirmation once the server's log supports it.
func (p *pass) deliver(a repository.ShopAttempt, purchaseID int64) error {
	w, inst := p.w, p.inst
	if a.RestartObservedAt == nil || a.UnstageVerifiedAt == nil {
		return p.pause("an attempt awaiting verification has no restart evidence in the ledger")
	}
	log, err := w.store.BootLog(p.ctx, inst.GameServerID, a.RestartObservedAt.Add(-time.Minute), time.Time{})
	if err != nil {
		return fmt.Errorf("read the boot log for %s: %w", a.AttemptID, err)
	}
	switch {
	case log.SpawnerError:
		return p.fail(a, "the server reported the Champion file missing or invalid at the restart: nothing spawned", repository.ShopAttemptEvidence{})
	case log.CentralEconomySeen:
		if err := w.buyers.OpenForDelivered(p.ctx, inst.OrganizationID, inst.InstallationID, purchaseID); err != nil {
			return fmt.Errorf("ask the buyer of %s: %w", a.AttemptID, err)
		}
		p.rep.Delivered = append(p.rep.Delivered, a.AttemptID)
	case w.now().Sub(*a.UnstageVerifiedAt) > LogEvidenceWait:
		return p.fail(a, "the server's log never showed that the restart finished: whether the item spawned is unknown", repository.ShopAttemptEvidence{})
	}
	return nil
}

// nitradoServer is a Server over the installation's Nitrado service.
type nitradoServer struct {
	remote  missionwrite.Remote
	binding capability.Binding
}

// NewNitradoServer binds the guarded read/write primitives to one installation's service.
func NewNitradoServer(remote missionwrite.Remote, inst repository.ShopAutoInstallation) Server {
	return nitradoServer{remote: remote, binding: capability.Binding{OrganizationID: inst.OrganizationID, InstallationID: inst.InstallationID,
		GameServerID: inst.GameServerID, NitradoServiceID: inst.NitradoServiceID}}
}

func (s nitradoServer) Inspect(ctx context.Context) (missionwrite.ArtifactState, error) {
	return missionwrite.InspectArtifact(ctx, s.remote, s.binding)
}

func (s nitradoServer) Write(ctx context.Context, st missionwrite.ArtifactState, payload []byte) (missionwrite.ArtifactWrite, error) {
	return missionwrite.WriteArtifact(ctx, s.remote, s.binding, st, payload)
}
