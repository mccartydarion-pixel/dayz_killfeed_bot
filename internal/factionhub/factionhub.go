// Package factionhub holds the pure rules of the Faction Hub (docs/FACTIONS.md):
// vocabulary, input validation and normalization, slug generation and the
// faction-role permission matrix. It has no database or HTTP code; the repository
// (internal/repository/faction_hub_repository.go) applies these rules inside its
// transactions and the handlers (internal/app/saas_api_factions.go) map the typed
// errors to HTTP.
//
// This is deliberately separate from internal/factions, the Discord-side faction
// system (guild + DayZ player scoped, wars/events/stats). The Hub is scoped to an
// organization + installation (+ DayZ server) and to website users.
package factionhub

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Built-in faction roles. The primary role of a member; LEADER > OFFICER > MEMBER.
const (
	RoleLeader  = "LEADER"
	RoleOfficer = "OFFICER"
	RoleMember  = "MEMBER"
)

// Recruitment statuses.
const (
	RecruitmentOpen       = "OPEN"        // players can submit applications
	RecruitmentInviteOnly = "INVITE_ONLY" // applications cannot be freely submitted
	RecruitmentClosed     = "CLOSED"      // no recruitment
)

// Application statuses. Application rows are never deleted; every outcome is a status.
const (
	ApplicationPending   = "PENDING"
	ApplicationAccepted  = "ACCEPTED"
	ApplicationDenied    = "DENIED"
	ApplicationWithdrawn = "WITHDRAWN"
	// ApplicationCancelled: closed by the system, not by a person - the applicant
	// joined (or founded) a faction on the installation through another route.
	ApplicationCancelled = "CANCELLED"
)

// Field limits (in runes).
const (
	MinNameLen        = 3
	MaxNameLen        = 32
	MinTagLen         = 2
	MaxTagLen         = 5
	MaxDescriptionLen = 500
	MaxMessageLen     = 500
	MaxRequirementLen = 500
	MaxSearchLen      = 50
	MaxSlugLen        = 40
	// DescriptionPreviewLen is the length of the directory's description preview.
	DescriptionPreviewLen = 140
	// MinimumAgeFloor is the lowest minimumAge a faction may display (Discord's own minimum).
	MinimumAgeFloor  = 13
	MaximumAgeCeil   = 99
	MaximumHoursCeil = 100000
)

// Typed errors. The repository returns these (never raw SQL errors) so the handlers
// can answer with a fixed, safe message.
var (
	// ErrNotFound: the faction/application/member does not exist within the given
	// organization + installation (deliberately the same answer as "belongs to another
	// tenant").
	ErrNotFound = errors.New("not found")
	// ErrForbidden: the acting user's faction role does not allow the action.
	ErrForbidden = errors.New("forbidden")

	ErrNoServer          = errors.New("installation has no DayZ server selected")
	ErrInstallationInert = errors.New("installation is suspended")

	ErrNameTaken         = errors.New("faction name already in use on this installation")
	ErrTagTaken          = errors.New("faction tag already in use on this installation")
	ErrAlreadyInFaction  = errors.New("user already belongs to a faction on this installation")
	ErrRecruitmentClosed = errors.New("faction is not accepting applications")
	ErrAlreadyApplied    = errors.New("a pending application already exists")
	ErrNotPending        = errors.New("application is not pending")
	ErrLeaderProtected   = errors.New("the faction leader cannot be changed or removed")
	ErrInvalidTransition = errors.New("role change is not allowed for this member")
)

// ValidationError lists user-facing problems with a request body.
type ValidationError struct{ Issues []string }

func (e *ValidationError) Error() string {
	return "invalid faction input: " + strings.Join(e.Issues, "; ")
}

// ValidRecruitmentStatus reports whether s is one of the three recruitment statuses.
func ValidRecruitmentStatus(s string) bool {
	return s == RecruitmentOpen || s == RecruitmentInviteOnly || s == RecruitmentClosed
}

// ValidRole reports whether s is a built-in role.
func ValidRole(s string) bool { return s == RoleLeader || s == RoleOfficer || s == RoleMember }

// ValidApplicationStatus reports whether s is a known application status (filter input).
func ValidApplicationStatus(s string) bool {
	switch s {
	case ApplicationPending, ApplicationAccepted, ApplicationDenied, ApplicationWithdrawn, ApplicationCancelled:
		return true
	}
	return false
}

// --- text normalization and validation ------------------------------------------------

// collapseSpaces trims and collapses every run of whitespace to one space.
func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// plainText normalizes free text: invalid UTF-8 is an error, control characters other
// than newline and tab are removed, CRLF becomes LF, and the ends are trimmed. Nothing
// is HTML-escaped or stripped: the text is stored as plain content and clients render
// it escaped.
func plainText(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String()), true
}

func nameRuneOK(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" -_.'&!", r)
}

// ValidateName normalizes and validates a faction name: 3-32 characters, letters,
// digits, spaces and - _ . ' & ! only (no mentions, markup or control characters), and
// at least two letters/digits.
func ValidateName(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", &ValidationError{Issues: []string{"name must be valid text"}}
	}
	name := collapseSpaces(raw)
	n := utf8.RuneCountInString(name)
	if n < MinNameLen || n > MaxNameLen {
		return "", &ValidationError{Issues: []string{fmt.Sprintf("name must be %d-%d characters", MinNameLen, MaxNameLen)}}
	}
	alnum := 0
	for _, r := range name {
		if !nameRuneOK(r) {
			return "", &ValidationError{Issues: []string{"name may only contain letters, numbers, spaces and - _ . ' & !"}}
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alnum++
		}
	}
	if alnum < 2 {
		return "", &ValidationError{Issues: []string{"name must contain letters or numbers"}}
	}
	return name, nil
}

// ValidateTag normalizes (upper-cases) and validates a faction tag: 2-5 ASCII letters
// or digits.
func ValidateTag(raw string) (string, error) {
	tag := strings.ToUpper(strings.TrimSpace(raw))
	if n := len(tag); n < MinTagLen || n > MaxTagLen {
		return "", &ValidationError{Issues: []string{fmt.Sprintf("tag must be %d-%d letters or numbers", MinTagLen, MaxTagLen)}}
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return "", &ValidationError{Issues: []string{"tag may only contain letters A-Z and numbers"}}
		}
	}
	return tag, nil
}

func validateFreeText(field, raw string, max int) (string, error) {
	s, ok := plainText(raw)
	if !ok {
		return "", &ValidationError{Issues: []string{field + " must be valid text"}}
	}
	if utf8.RuneCountInString(s) > max {
		return "", &ValidationError{Issues: []string{fmt.Sprintf("%s must be at most %d characters", field, max)}}
	}
	return s, nil
}

// ValidateDescription normalizes the faction description (plain text, at most 500 characters).
func ValidateDescription(raw string) (string, error) {
	return validateFreeText("description", raw, MaxDescriptionLen)
}

// ValidateMessage normalizes an application message (plain text, at most 500 characters).
func ValidateMessage(raw string) (string, error) {
	return validateFreeText("message", raw, MaxMessageLen)
}

// ValidateColor validates an optional #RRGGBB color. "" means "no color".
func ValidateColor(field, raw string) (string, error) {
	c := strings.TrimSpace(raw)
	if c == "" {
		return "", nil
	}
	if len(c) != 7 || c[0] != '#' {
		return "", &ValidationError{Issues: []string{field + " must be a #RRGGBB color"}}
	}
	for i := 1; i < 7; i++ {
		ch := c[i]
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') && !(ch >= 'A' && ch <= 'F') {
			return "", &ValidationError{Issues: []string{field + " must be a #RRGGBB color"}}
		}
	}
	return strings.ToUpper(c), nil
}

// NormalizeSearch trims a directory search term; ok is false when it is too long.
func NormalizeSearch(raw string) (string, bool) {
	s := collapseSpaces(raw)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxSearchLen {
		return "", false
	}
	return s, true
}

// Preview returns a single-line description preview of at most DescriptionPreviewLen runes.
func Preview(description string) string {
	s := collapseSpaces(description)
	if utf8.RuneCountInString(s) <= DescriptionPreviewLen {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:DescriptionPreviewLen-1])) + "…"
}

// --- settings (recruitment requirements) ----------------------------------------------

// Settings are display/application requirements only. Nothing here is verified against
// a real-world identity, and none of it is enforced when applying.
type Settings struct {
	MinimumHours       *int   `json:"minimumHours"`
	MinimumAge         *int   `json:"minimumAge"`
	PvPRequired        bool   `json:"pvpRequired"`
	BuilderNeeded      bool   `json:"builderNeeded"`
	MicRequired        bool   `json:"micRequired"`
	CustomRequirements string `json:"customRequirements"`
}

// ValidateSettings normalizes and validates a Settings value.
func ValidateSettings(in Settings) (Settings, error) {
	var issues []string
	out := in
	if in.MinimumHours != nil && (*in.MinimumHours < 0 || *in.MinimumHours > MaximumHoursCeil) {
		issues = append(issues, fmt.Sprintf("minimumHours must be between 0 and %d", MaximumHoursCeil))
	}
	if in.MinimumAge != nil && (*in.MinimumAge < MinimumAgeFloor || *in.MinimumAge > MaximumAgeCeil) {
		issues = append(issues, fmt.Sprintf("minimumAge must be between %d and %d", MinimumAgeFloor, MaximumAgeCeil))
	}
	text, err := validateFreeText("customRequirements", in.CustomRequirements, MaxRequirementLen)
	if err != nil {
		var v *ValidationError
		if errors.As(err, &v) {
			issues = append(issues, v.Issues...)
		}
	}
	out.CustomRequirements = text
	if len(issues) > 0 {
		return Settings{}, &ValidationError{Issues: issues}
	}
	return out, nil
}

// --- slugs ------------------------------------------------------------------------------

// Slugify makes a readable URL slug from a faction name: "UNIT ZERO" -> "unit-zero".
// Only [a-z0-9] survive (letters outside ASCII are dropped), runs of anything else become
// one hyphen, and the result is trimmed and length-capped. A name with no ASCII
// letters/digits falls back to "faction".
func Slugify(name string) string {
	var b strings.Builder
	dash := true // no leading hyphen
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > MaxSlugLen {
		s = strings.Trim(s[:MaxSlugLen], "-")
	}
	if s == "" {
		return "faction"
	}
	return s
}

// SlugCandidate returns the n-th candidate for a base slug: the base itself for n<=1,
// then base-2, base-3, ... (the base is shortened so the suffix still fits).
func SlugCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := fmt.Sprintf("-%d", n)
	if len(base)+len(suffix) > MaxSlugLen {
		base = strings.Trim(base[:MaxSlugLen-len(suffix)], "-")
	}
	return base + suffix
}

// --- permissions ----------------------------------------------------------------------

// CanManageApplications: LEADER and OFFICER may list, accept and deny applications.
func CanManageApplications(role string) bool { return role == RoleLeader || role == RoleOfficer }

// CanEditFaction: only the LEADER edits the profile, recruitment status and requirements.
func CanEditFaction(role string) bool { return role == RoleLeader }

// CanChangeRoles: only the LEADER promotes and demotes.
func CanChangeRoles(role string) bool { return role == RoleLeader }

// CanRemove reports whether actorRole may remove a member holding targetRole. The LEADER
// may remove MEMBER and OFFICER; an OFFICER may remove MEMBER only; nobody removes the
// LEADER (ownership transfer is a later phase).
func CanRemove(actorRole, targetRole string) bool {
	switch actorRole {
	case RoleLeader:
		return targetRole == RoleMember || targetRole == RoleOfficer
	case RoleOfficer:
		return targetRole == RoleMember
	}
	return false
}

// PromotedRole returns the role a member holding role moves to when promoted
// (MEMBER -> OFFICER). ok is false when the role cannot be promoted.
func PromotedRole(role string) (string, bool) {
	if role == RoleMember {
		return RoleOfficer, true
	}
	return "", false
}

// DemotedRole returns the role an OFFICER moves to when demoted (-> MEMBER).
func DemotedRole(role string) (string, bool) {
	if role == RoleOfficer {
		return RoleMember, true
	}
	return "", false
}

// RoleRank orders roles for display (lower = more senior); unknown roles sort last.
func RoleRank(role string) int {
	switch role {
	case RoleLeader:
		return 0
	case RoleOfficer:
		return 1
	case RoleMember:
		return 2
	}
	return 3
}
