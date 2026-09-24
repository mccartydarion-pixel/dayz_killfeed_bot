package livesync

import (
	"path"
	"regexp"
	"strings"
	"time"
)

// SourceInfo is what a file's path alone establishes. Only real, observed naming is recognized
// (docs/CHAMPION_LIVE_SYNC.md "Source inventory"); anything else is FamilyUnknown.
type SourceInfo struct {
	Name        string
	Family      string
	CanonicalID string // mount-independent identity: ftproot/ and noftp/ copies of a file share it
	// FileLocalStart is the server-local time encoded in the filename, when the family encodes one.
	// It is boot EVIDENCE, not proof: CorrelateBoots prefers in-file startup evidence.
	FileLocalStart *time.Time
}

var (
	dayzServerFileRe = regexp.MustCompile(`^DayZServer_[A-Za-z0-9]+_x64_(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})\.(ADM|RPT)$`)
	stampedLogRe     = regexp.MustCompile(`^(script|crash)_(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})\.log$`)
	mountMarkers     = []string{"/noftp/", "/ftproot/"}
)

// CanonicalSourceID strips the mount prefix so the same logical file seen through ftproot/ and
// noftp/ has one identity ("dayzps/config/X.ADM", "restart.log"). A path with no known mount keeps
// its full path.
func CanonicalSourceID(p string) string {
	for _, m := range mountMarkers {
		if i := strings.Index(p, m); i >= 0 {
			return p[i+len(m):]
		}
	}
	return p
}

// ClassifySource classifies one file-server path.
func ClassifySource(p string) SourceInfo {
	name := path.Base(p)
	info := SourceInfo{Name: name, Family: FamilyUnknown, CanonicalID: CanonicalSourceID(p)}
	if m := dayzServerFileRe.FindStringSubmatch(name); m != nil {
		info.Family = map[string]string{"ADM": FamilyADM, "RPT": FamilyRPT}[m[2]]
		info.FileLocalStart = parseStamp(m[1])
		return info
	}
	if m := stampedLogRe.FindStringSubmatch(name); m != nil {
		info.Family = map[string]string{"script": FamilyScript, "crash": FamilyCrash}[m[1]]
		info.FileLocalStart = parseStamp(m[2])
		return info
	}
	switch strings.ToLower(name) {
	case "restart.log":
		info.Family = FamilyRestart
	case "ban.txt":
		info.Family = FamilyBanList
	case "whitelist.txt":
		info.Family = FamilyWhitelist
	}
	return info
}

func parseStamp(s string) *time.Time {
	t, err := time.Parse("2006-01-02_15-04-05", s)
	if err != nil {
		return nil
	}
	return &t
}

var (
	redactIDRe   = regexp.MustCompile(`\b(d?p?id)[=\s]+[^\s),]+`)
	redactIPRe   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	redactHostRe = regexp.MustCompile(`ni\d{4,}_\d+`)
)

// Redact bounds and scrubs an evidence excerpt: player/platform ids, IP addresses and the Nitrado
// service account name are removed; at most 240 characters are kept.
func Redact(line string) string {
	s := strings.TrimSpace(line)
	s = redactIDRe.ReplaceAllString(s, "$1=<redacted>")
	s = redactIPRe.ReplaceAllString(s, "<ip>")
	s = redactHostRe.ReplaceAllString(s, "<service>")
	if r := []rune(s); len(r) > 240 {
		s = string(r[:240])
	}
	return s
}
