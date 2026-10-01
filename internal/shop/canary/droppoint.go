package canary

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// Drop-point verification statuses.
const (
	DropPointVerified   = "VERIFIED_CURRENT_SESSION" // usable for a canary plan
	DropPointRejected   = "REJECTED"
	DropPointMaxAgeDflt = 20 * time.Minute // a PLAYER_LIST row is written every five minutes
)

// Source kinds that may carry a drop point: only ADM lines the server itself wrote.
const (
	SourceADMPlayerList = "ADM_PLAYER_LIST"
	SourceADMEvent      = "ADM_EVENT"
)

var (
	ErrDropPointTenant        = errors.New("the drop point belongs to a different organization/installation")
	ErrDropPointMap           = errors.New("the drop point is not on the installation's supported map")
	ErrDropPointCoordinates   = errors.New("the drop point coordinates are missing or outside the map")
	ErrDropPointAltitude      = errors.New("the drop point has no source altitude: an altitude is never invented")
	ErrDropPointNoSource      = errors.New("the drop point has no source identity (ADM file and offset): it cannot be re-verified")
	ErrDropPointSourceKind    = errors.New("the drop point is not an ADM observation written by the server")
	ErrDropPointPreviousBoot  = errors.New("the drop point comes from a previous boot: a position from another session is never current")
	ErrDropPointSessionEnded  = errors.New("the current boot session has ended: no position is current until a new boot is accepted")
	ErrDropPointStale         = errors.New("the drop point observation is too old")
	ErrDropPointFutureTime    = errors.New("the drop point observation is in the future")
	ErrDropPointNoSessionInfo = errors.New("the installation's current boot session is unknown")
)

// DropPoint is one candidate drop position, exactly as the source recorded it. ADM writes
// <x, z, altitude>; the stored row keeps X, Z and Y (altitude) separately.
type DropPoint struct {
	OrganizationID, InstallationID int64
	MapKey                         string
	X, Z                           float64
	AltitudeY                      *float64
	SourceKind                     string
	SourceFile                     string // canonical ADM file (the boot identity)
	SourceOffset                   int64
	SourceLocalTime                time.Time // zone-less server-local time from the ADM line
	ObservedAt                     time.Time // UTC
}

// CurrentSession is the installation's source-authoritative boot session (server_adm_sessions).
type CurrentSession struct {
	ADMFile string
	EndedAt *time.Time
}

// DropPointCheck is the verification result.
type DropPointCheck struct {
	Status string
	Reason string
	Pos    [3]float64 // spawner order: x, altitude, z
}

// VerifyDropPoint accepts a drop point only when it is a server-written ADM observation with an
// altitude, on the installation's map, from the CURRENT boot session (which has not ended), and
// younger than maxAge.
func VerifyDropPoint(dp DropPoint, orgID, instID int64, mapKey string, sess *CurrentSession, now time.Time, maxAge time.Duration) (DropPointCheck, error) {
	reject := func(err error) (DropPointCheck, error) {
		return DropPointCheck{Status: DropPointRejected, Reason: err.Error()}, err
	}
	if dp.OrganizationID != orgID || dp.InstallationID != instID {
		return reject(ErrDropPointTenant)
	}
	m, ok := dayzmap.Lookup(dp.MapKey)
	if !ok || dp.MapKey != mapKey {
		return reject(ErrDropPointMap)
	}
	if !finite(dp.X) || !finite(dp.Z) || !m.Contains(dp.X, dp.Z) {
		return reject(ErrDropPointCoordinates)
	}
	if dp.AltitudeY == nil || !finite(*dp.AltitudeY) || *dp.AltitudeY < nitradodelivery.MinAltitude || *dp.AltitudeY > nitradodelivery.MaxAltitude {
		return reject(ErrDropPointAltitude)
	}
	if dp.SourceKind != SourceADMPlayerList && dp.SourceKind != SourceADMEvent {
		return reject(ErrDropPointSourceKind)
	}
	if dp.SourceFile == "" || dp.SourceOffset <= 0 || !strings.HasSuffix(dp.SourceFile, ".ADM") {
		return reject(ErrDropPointNoSource)
	}
	if sess == nil || sess.ADMFile == "" {
		return reject(ErrDropPointNoSessionInfo)
	}
	if sess.EndedAt != nil {
		return reject(ErrDropPointSessionEnded)
	}
	if dp.SourceFile != sess.ADMFile {
		return reject(ErrDropPointPreviousBoot)
	}
	if maxAge <= 0 {
		maxAge = DropPointMaxAgeDflt
	}
	switch age := now.Sub(dp.ObservedAt); {
	case dp.ObservedAt.IsZero() || age > maxAge:
		return reject(ErrDropPointStale)
	case age < -time.Minute:
		return reject(ErrDropPointFutureTime)
	}
	pos := SpawnerPosFromADM(dp.X, dp.Z, *dp.AltitudeY)
	return DropPointCheck{Status: DropPointVerified, Pos: pos,
		Reason: fmt.Sprintf("ADM %s @%d, current session, %s old", dp.SourceFile, dp.SourceOffset, now.Sub(dp.ObservedAt).Round(time.Second))}, nil
}

// SpawnerPosFromADM converts an ADM position <x, z, altitude> into the object spawner's
// pos [x, altitude, z].
func SpawnerPosFromADM(x, z, altitude float64) [3]float64 { return [3]float64{x, altitude, z} }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
