package canary

// Owner gates, in execution order. Each is a separate, explicit approval; approving one never implies
// another. Nothing in this package performs a gate - it only describes it.
//
// Order rationale: the Champion file is created EMPTY before the configuration references it (A before
// B), so a scheduled restart between any two gates meets either no reference or a reference to an
// existing file that spawns nothing. The single item is written only at E, with an operator present,
// and is replaced by the empty file again at G.
const (
	GateA        = "A"         // create the empty Champion spawner file
	GateB        = "B"         // reference it in cfggameplay.json
	GateC        = "C"         // deploy the durable attempt ledger migration
	GateD        = "D"         // create the canary purchase through the existing Shop flow
	GateE        = "E"         // stage BandageDressing x1 (replace the empty file)
	GateF        = "F"         // first restart (scheduled or manual) and observation
	GateG        = "G"         // unstage (restore the empty file)
	GateH        = "H"         // second restart and in-game no-respawn check
	GateI        = "I"         // final fulfillment
	GateLostCity = "LOST_CITY" // optional, independent of the Shop canary
)

// LedgerMigrationName is the durable attempt ledger this canary consumes. It is implemented and
// registered in PR #97 (internal/database/shop_attempts.go), not here: this package never defines a
// migration of its own.
const LedgerMigrationName = "0054_shop_delivery_attempts"

// WriteCapability stays UNVERIFIED until an authorized live upload has been read back with the
// expected SHA-256. A listed FILEBROWSER_WRITE role is documentation, not proof.
const WriteCapability = "UNVERIFIED"

// Gate is one approval request.
type Gate struct {
	ID            string
	Title         string
	Action        string
	Paths         []string // mission-relative paths it writes (none for record/restart gates)
	Writes        bool     // performs a production file write
	Records       bool     // performs a production database write or deployment
	Restarts      bool     // needs a server start
	ItemExposure  bool     // the single item is (or may be) on the server during this gate
	Preconditions []string
	Evidence      []string // what must be shown before the next gate may be requested
	Rollback      string
}

// Gates returns the canary gates in execution order (the Lost City decision is separate: LostCityGate).
func Gates() []Gate {
	art := "champion/champion_shop_delivery.json"
	return []Gate{
		{ID: GateA, Title: "Create the empty Champion spawner file", Writes: true, Paths: []string{art},
			Action: "upload the empty spawner file (EmptyArtifact) to " + art + "; nothing references it yet",
			Preconditions: []string{art + " is absent (or already exactly the empty file: then skip)",
				"whether an upload creates the champion/ directory is UNVERIFIED: if it does not, stop - the owner creates the folder in the Nitrado file browser"},
			Evidence: []string{"read-back SHA-256 equals the empty-file SHA-256", "write capability becomes VERIFIED LIVE only with this read-back"},
			Rollback: "leave it (an unreferenced file is never read) or the owner deletes it in the Nitrado file browser"},
		{ID: GateB, Title: "Reference the Champion file in cfggameplay.json", Writes: true, Paths: []string{"cfggameplay.json", "champion/backup/"},
			Action: "back up cfggameplay.json, then upload the patch that appends " + art + " to objectSpawnersArr (all other entries untouched)",
			Preconditions: []string{"Gate A verified and CheckReferencePrecondition passes on a fresh read",
				"the live cfggameplay.json still has the reviewed current SHA-256", "the owner does not edit the file during the window"},
			Evidence: []string{"read-back SHA-256 equals the proposed SHA-256",
				"the next boot (a scheduled one is fine: the file is empty) reaches CE init with no [::SpawnObjects] error naming the Champion file"},
			Rollback: "upload the verified backup and confirm the original SHA-256; effective at the next start"},
		{ID: GateC, Title: "Deploy the durable attempt ledger", Records: true,
			Action:        "merge and deploy PR #97: migration " + LedgerMigrationName + " (the durable attempt ledger) - no file write",
			Preconditions: []string{"the migration's integration test is green in CI", "deployed independently of any file write"},
			Evidence:      []string{"schema_migrations lists the migration", "refund and fulfil of a delivery without attempts behave exactly as before"},
			Rollback:      "additive: drop the triggers, then the tables (the migration modifies no Shop row)"},
		{ID: GateD, Title: "Create the canary purchase (existing Shop flow)", Records: true,
			Action: "the owner buys a BandageDressing canary product (1 point, MANUAL_COORDINATE) at the drop point's X/Z through the normal Shop purchase flow",
			Preconditions: []string{"a fresh drop point from the current boot, the owner standing at the spot (VerifyDropPoint)",
				"the canary product exists: 1 point, purchase_limit 1, active only for the purchase window", "the delivery map is chernarusplus"},
			Evidence: []string{"purchase PENDING_FULFILLMENT and delivery MANUAL_READY within 0.5 m of the drop point", "a real delivery id (never 0)"},
			Rollback: "refund through the existing refund flow (allowed: no attempt is on the server yet)"},
		{ID: GateE, Title: "Stage BandageDressing x1", Writes: true, ItemExposure: true, Paths: []string{art},
			Action: "replace the empty Champion file with the reviewed single-item content for the real attempt",
			Preconditions: []string{"Gates A-D verified",
				"StagingReadiness passes: operator available for the whole exposure window, current boot older than the quiet period, no pending restart.log pre-start, drop point still fresh",
				"attempt PLAN_CREATED -> FILE_PREPARED recorded (refunds are blocked from here)", "the Champion file still reads back as the empty file"},
			Evidence: []string{"read-back SHA-256 equals the staged SHA-256", "attempt FILE_STAGED -> AWAITING_RESTART"},
			Rollback: "before any start: restore the empty file, verify, attempt -> UNSTAGED (then the purchase may be refunded)"},
		{ID: GateF, Title: "First restart and observation", Restarts: true, ItemExposure: true,
			Action:        "a restart - owner-triggered (recommended: shortest exposure) or the next scheduled one; the operator records each milestone",
			Preconditions: []string{"Gate E verified", "the operator is present"},
			Evidence: []string{"restart initiated (restart.log or owner)", "new boot accepted by boot authority",
				"spawner processing attempted (CE init reached, no Champion spawner error)", "item observed in game at the drop point and picked up (named observer)"},
			Rollback: "none: a started server may have spawned the item; go to Gate G at once"},
		{ID: GateG, Title: "Unstage", Writes: true, ItemExposure: true, Paths: []string{art},
			Action:        "restore the empty Champion file and verify it before any further start",
			Preconditions: []string{"attempt UNSTAGE_REQUIRED", "the file still has the staged SHA-256 (otherwise stop and review)"},
			Evidence:      []string{"read-back of the empty-file SHA-256, timestamped before the next boot starts", "attempt -> VERIFICATION_REQUIRED"},
			Rollback:      "not applicable: the empty file is the safe state"},
		{ID: GateH, Title: "Second restart and no-respawn check", Restarts: true,
			Action:        "one more start with the entry removed; the operator checks the drop point in game",
			Preconditions: []string{"Gate G verified"},
			Evidence: []string{"a new boot accepted after the verified unstage, CE init reached, no Champion spawner error",
				"in game: no new BandageDressing at the drop point (the collected original is not required to be there)"},
			Rollback: "a new bandage appeared: FAILED_REVIEW"},
		{ID: GateI, Title: "Final fulfillment", Records: true,
			Action:        "record VERIFICATION_REQUIRED -> FULFILLED and fulfil the delivery in the same transaction",
			Preconditions: []string{"attempt VERIFICATION_REQUIRED", "Gate H clean", "Fulfill accepts the physical confirmation"},
			Evidence:      []string{"attempt FULFILLED with verified_by; delivery and purchase FULFILLED"},
			Rollback:      "item never observed: FAILED_REVIEW, never an automatic retry"},
	}
}

// LostCityGate is the separate, optional map decision. It is not part of the canary and does not
// depend on it.
func LostCityGate() Gate {
	return Gate{ID: GateLostCity, Title: "Re-enable The Lost City (optional, owner)", Writes: true, Paths: []string{"cfggameplay.json"},
		Action:        "apply ProposeLostCityRestore to the then-current cfggameplay.json",
		Preconditions: []string{"owner decision", "custom/The_Lost_City.json re-verified", "never combined with a Shop gate"},
		Evidence:      []string{"read-back SHA-256 equals the proposal", "the next boot's RPT has no [::SpawnObjects] error for the Lost City file"},
		Rollback:      "restore the verified backup"}
}

// UploadStep is one step of the write sequence. Kind is READ or WRITE; only WRITE steps need a gate.
type UploadStep struct {
	N       int
	Kind    string
	Action  string
	AbortIf string
}

// UploadSequence is the ordered, verified write of one file. Nitrado has no conditional write
// (no If-Match, no version check): the sequence narrows the race to the seconds between the
// re-read (step 4) and the upload (step 5) and detects it afterwards (step 6).
func UploadSequence(target, expectBefore, expectAfter, backup string) []UploadStep {
	return []UploadStep{
		{1, "READ", "download " + target + " and compute SHA-256", "the digest is not " + expectBefore + " (or the file unexpectedly exists/does not exist)"},
		{2, "WRITE", "upload the same bytes to " + backup, "the upload fails"},
		{3, "READ", "download " + backup + " and compute SHA-256", "the digest is not " + expectBefore},
		{4, "READ", "download " + target + " again immediately before writing", "the digest changed since step 1 (someone else edited it)"},
		{5, "WRITE", "request a single-use upload token for " + target + " and POST the new content", "either request fails: go to step 6 anyway (a failed response does not prove nothing was written)"},
		{6, "READ", "download " + target + " and compute SHA-256", "the digest is neither " + expectAfter + " nor " + expectBefore + ": restore " + backup + " and verify"},
		{7, "RECORD", "record the attempt transition with both digests only after step 6 verified " + expectAfter, "the record fails: the next reconcile reads the file (Reconcile)"},
	}
}
