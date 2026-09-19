package factionhub

import (
	"errors"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"UNIT ZERO":             "unit-zero",
		"  Unit   Zero  ":       "unit-zero",
		"Unit-Zero":             "unit-zero",
		"Bravo 6 & Co.":         "bravo-6-co",
		"!!":                    "faction",
		"Ünïcode Squad":         "n-code-squad", // non-ASCII letters are dropped, never transliterated
		"日本語":                   "faction",
		"a_b_c":                 "a-b-c",
		"---x---":               "x",
		strings.Repeat("a", 80): strings.Repeat("a", MaxSlugLen),
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Slugify(strings.Repeat("ab-", 30)); len(got) > MaxSlugLen || strings.HasSuffix(got, "-") {
		t.Errorf("long slug must be capped and trimmed, got %q", got)
	}
}

func TestSlugCandidateSuffixesAndStaysWithinLimit(t *testing.T) {
	if got := SlugCandidate("unit-zero", 1); got != "unit-zero" {
		t.Fatalf("first candidate is the base, got %q", got)
	}
	if got := SlugCandidate("unit-zero", 2); got != "unit-zero-2" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", MaxSlugLen)
	if got := SlugCandidate(long, 12); len(got) > MaxSlugLen || !strings.HasSuffix(got, "-12") {
		t.Fatalf("suffixed slug must still fit the limit, got %q (%d)", got, len(got))
	}
}

func TestValidateName(t *testing.T) {
	good := map[string]string{"Unit Zero": "Unit Zero", "  Unit    Zero ": "Unit Zero", "R.O.G.U.E": "R.O.G.U.E", "Bravo-6": "Bravo-6", "Ünï Squad": "Ünï Squad"}
	for in, want := range good {
		got, err := ValidateName(in)
		if err != nil || got != want {
			t.Errorf("ValidateName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "ab", strings.Repeat("x", MaxNameLen+1), "@everyone", "Unit <b>Zero</b>", "Unit\x00Zero", "!!!", "..", "Unit`Zero", "a\xffb c", "Unit#Zero", "<script>alert(1)</script>"}
	for _, in := range bad {
		if _, err := ValidateName(in); err == nil {
			t.Errorf("ValidateName(%q) must fail", in)
		} else {
			var v *ValidationError
			if !errors.As(err, &v) {
				t.Errorf("ValidateName(%q) error must be a ValidationError, got %T", in, err)
			}
		}
	}
	// Newlines collapse to a space rather than being stored inside a name.
	if got, err := ValidateName("Unit\nZero"); err != nil || got != "Unit Zero" {
		t.Errorf("newline between words should collapse: %q, %v", got, err)
	}
}

func TestValidateTag(t *testing.T) {
	for in, want := range map[string]string{"uz": "UZ", " Abc1 ": "ABC1", "ZERO5": "ZERO5"} {
		if got, err := ValidateTag(in); err != nil || got != want {
			t.Errorf("ValidateTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "ABCDEF", "A B", "A-B", "@ab", "ÄB", "<b>"} {
		if _, err := ValidateTag(in); err == nil {
			t.Errorf("ValidateTag(%q) must fail", in)
		}
	}
}

func TestPlainTextIsStoredNotEscaped(t *testing.T) {
	got, err := ValidateDescription("  We <3 base building & \"raids\" <script>x</script>\r\nLine two\x07\x00  ")
	if err != nil {
		t.Fatal(err)
	}
	// No HTML processing: the markup is kept verbatim as text (clients escape on render);
	// only control characters go and line endings normalize.
	want := "We <3 base building & \"raids\" <script>x</script>\nLine two"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := ValidateDescription(strings.Repeat("é", MaxDescriptionLen+1)); err == nil {
		t.Fatal("over-long description must fail (counted in runes)")
	}
	if _, err := ValidateDescription(strings.Repeat("é", MaxDescriptionLen)); err != nil {
		t.Fatalf("limit is inclusive: %v", err)
	}
	if _, err := ValidateMessage("bad \xff utf8"); err == nil {
		t.Fatal("invalid UTF-8 must fail")
	}
}

func TestValidateColor(t *testing.T) {
	if got, err := ValidateColor("primaryColor", " #aabbcc "); err != nil || got != "#AABBCC" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := ValidateColor("primaryColor", ""); err != nil || got != "" {
		t.Fatalf("empty clears: %q, %v", got, err)
	}
	for _, in := range []string{"red", "#abc", "#gggggg", "aabbcc", "#aabbccdd", "url(x)"} {
		if _, err := ValidateColor("primaryColor", in); err == nil {
			t.Errorf("ValidateColor(%q) must fail", in)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	ten, seventeen := 10, 17
	got, err := ValidateSettings(Settings{MinimumHours: &ten, MinimumAge: &seventeen, PvPRequired: true, CustomRequirements: "  Mic\r\nand patience "})
	if err != nil || got.CustomRequirements != "Mic\nand patience" || !got.PvPRequired {
		t.Fatalf("got %+v, %v", got, err)
	}
	neg, young, huge := -1, 12, MaximumHoursCeil+1
	for name, s := range map[string]Settings{
		"negative hours": {MinimumHours: &neg}, "too many hours": {MinimumHours: &huge}, "too young": {MinimumAge: &young},
		"long text": {CustomRequirements: strings.Repeat("x", MaxRequirementLen+1)},
	} {
		if _, err := ValidateSettings(s); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
	// Several problems are all reported.
	_, err = ValidateSettings(Settings{MinimumHours: &neg, MinimumAge: &young})
	var v *ValidationError
	if !errors.As(err, &v) || len(v.Issues) != 2 {
		t.Fatalf("want 2 issues, got %v", err)
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("  short   text "); got != "short text" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("word ", 100)
	got := Preview(long)
	if n := len([]rune(got)); n > DescriptionPreviewLen || !strings.HasSuffix(got, "…") {
		t.Fatalf("preview must be capped with an ellipsis, got %d runes: %q", n, got)
	}
}

func TestNormalizeSearch(t *testing.T) {
	if got, ok := NormalizeSearch("  unit   zero "); !ok || got != "unit zero" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := NormalizeSearch(strings.Repeat("a", MaxSearchLen+1)); ok {
		t.Fatal("over-long search must be rejected")
	}
}

func TestRecruitmentAndStatusVocabulary(t *testing.T) {
	for _, s := range []string{RecruitmentOpen, RecruitmentInviteOnly, RecruitmentClosed} {
		if !ValidRecruitmentStatus(s) {
			t.Errorf("%s must be valid", s)
		}
	}
	for _, s := range []string{"", "open", "PUBLIC", "INVITE"} {
		if ValidRecruitmentStatus(s) {
			t.Errorf("%q must be invalid (case-sensitive; the API upper-cases first)", s)
		}
	}
	for _, s := range []string{ApplicationPending, ApplicationAccepted, ApplicationDenied, ApplicationWithdrawn, ApplicationCancelled} {
		if !ValidApplicationStatus(s) {
			t.Errorf("%s must be a valid application status", s)
		}
	}
	if ValidApplicationStatus("APPROVED") || !ValidRole(RoleOfficer) || ValidRole("OWNER") {
		t.Fatal("vocabulary mismatch")
	}
}

func TestPermissionMatrix(t *testing.T) {
	roles := []string{RoleLeader, RoleOfficer, RoleMember, ""}
	wantManage := map[string]bool{RoleLeader: true, RoleOfficer: true}
	wantEdit := map[string]bool{RoleLeader: true}
	for _, r := range roles {
		if CanManageApplications(r) != wantManage[r] {
			t.Errorf("CanManageApplications(%q)", r)
		}
		if CanEditFaction(r) != wantEdit[r] || CanChangeRoles(r) != wantEdit[r] {
			t.Errorf("edit/role rights for %q", r)
		}
	}
	type pair struct{ actor, target string }
	want := map[pair]bool{
		{RoleLeader, RoleMember}: true, {RoleLeader, RoleOfficer}: true, {RoleLeader, RoleLeader}: false,
		{RoleOfficer, RoleMember}: true, {RoleOfficer, RoleOfficer}: false, {RoleOfficer, RoleLeader}: false,
		{RoleMember, RoleMember}: false, {RoleMember, RoleOfficer}: false, {RoleMember, RoleLeader}: false,
		{"", RoleMember}: false,
	}
	for p, w := range want {
		if got := CanRemove(p.actor, p.target); got != w {
			t.Errorf("CanRemove(%s, %s) = %v, want %v", p.actor, p.target, got, w)
		}
	}
}

func TestRoleTransitions(t *testing.T) {
	if r, ok := PromotedRole(RoleMember); !ok || r != RoleOfficer {
		t.Fatal("MEMBER promotes to OFFICER")
	}
	for _, r := range []string{RoleOfficer, RoleLeader, ""} {
		if _, ok := PromotedRole(r); ok {
			t.Errorf("%q must not be promotable (no officer->leader, no leader promotion)", r)
		}
	}
	if r, ok := DemotedRole(RoleOfficer); !ok || r != RoleMember {
		t.Fatal("OFFICER demotes to MEMBER")
	}
	for _, r := range []string{RoleMember, RoleLeader, ""} {
		if _, ok := DemotedRole(r); ok {
			t.Errorf("%q must not be demotable", r)
		}
	}
	if !(RoleRank(RoleLeader) < RoleRank(RoleOfficer) && RoleRank(RoleOfficer) < RoleRank(RoleMember) && RoleRank(RoleMember) < RoleRank("x")) {
		t.Fatal("role ranks must order LEADER < OFFICER < MEMBER < unknown")
	}
}
