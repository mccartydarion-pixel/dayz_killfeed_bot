//go:build integration

package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 0068: the attempt ledger accepts the legacy champion/ location and the custom/ location,
// nothing else; the path stays immutable per attempt.
func TestShopAttemptArtifactPaths(t *testing.T) {
	w := newAttemptWorld(t)
	create := func(o *order, path string) (ShopAttempt, error) {
		n := o.attemptN + 1
		return w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, DeliveryID: o.delivery, Attempt: n,
			AttemptID: fmt.Sprintf("champion:d%d:a%d", o.delivery, n), Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1,
			PosX: 4621.1, PosY: 319.6, PosZ: 8397.2, DropSourceFile: "dayzps/config/DayZServer_PS4_x64_2026-09-29_08-23-54.ADM", DropSourceOffset: 853,
			ArtifactPath: path}, "worker-test")
	}

	// Default (empty) keeps the existing behaviour: the legacy location.
	legacy, err := create(w.order(w.a), "")
	must(t, err)
	if legacy.ArtifactPath != ShopAttemptLegacyArtifactPath {
		t.Fatalf("default path: %s", legacy.ArtifactPath)
	}
	// The custom/ location is accepted.
	custom, err := create(w.order(w.a), ShopAttemptCustomArtifactPath)
	must(t, err)
	if custom.ArtifactPath != ShopAttemptCustomArtifactPath {
		t.Fatalf("custom path: %s", custom.ArtifactPath)
	}
	// Anything else is refused by the repository before any write...
	for _, bad := range []string{"custom/other.json", "champion/../custom/champion_shop_delivery.json", "/custom/champion_shop_delivery.json", "CUSTOM/champion_shop_delivery.json"} {
		o := w.order(w.a)
		if _, err := create(o, bad); !errors.Is(err, ErrShopAttemptArtifactPath) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// ...and by the database itself. Raw statements carry an actor exactly like the repository does
	// (0054 refuses any ledger change without champion.actor), so the error below is the CHECK's.
	withActor := func(sql string, args ...any) error {
		tx, err := w.db.Pool.Begin(w.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(w.ctx)
		if _, err := tx.Exec(w.ctx, `SELECT set_config('champion.actor', 'artifact-path-test', true), set_config('champion.evidence', 'test', true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(w.ctx, sql, args...); err != nil {
			return err
		}
		return tx.Commit(w.ctx)
	}
	o := w.order(w.a)
	err = withActor(`INSERT INTO shop_delivery_attempts(organization_id, installation_id, delivery_id, attempt, attempt_id, fingerprint, artifact_path,
 class_name, quantity, pos_x, pos_y, pos_z, drop_source_file, drop_source_offset)
VALUES($1,$2,$3,1,$4,$5,'custom/other.json','BandageDressing',1,4621.1,319.6,8397.2,'x.ADM',853)`,
		o.f.OrgID, o.f.InstallationID, o.delivery, fmt.Sprintf("champion:d%d:a1", o.delivery), strings.Repeat("ab", 32))
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "shop_delivery_attempts_artifact_path_allowed" {
		t.Fatalf("database CHECK: %v", err)
	}
	// Immutable per attempt: a recorded legacy path can never be rewritten to custom/ (0054 identity trigger).
	err = withActor(`UPDATE shop_delivery_attempts SET artifact_path=$1 WHERE id=$2`, ShopAttemptCustomArtifactPath, legacy.ID)
	if !errors.As(err, &pe) || pe.Code != "SA422" || !strings.Contains(pe.Message, "immutable") {
		t.Fatalf("immutability: %v", err)
	}
	// Exactly one artifact_path CHECK remains, and it is the named 0068 constraint.
	var n int
	var name string
	must(t, w.db.Pool.QueryRow(w.ctx, `SELECT COUNT(*), MIN(conname) FROM pg_constraint
 WHERE conrelid = 'shop_delivery_attempts'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%artifact_path%'`).Scan(&n, &name))
	if n != 1 || name != "shop_delivery_attempts_artifact_path_allowed" {
		t.Fatalf("constraints: %d %s", n, name)
	}
}
