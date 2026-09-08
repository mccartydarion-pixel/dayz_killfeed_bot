package killfeed

import (
	"regexp"
	"strings"
)

// gameplayTerms are substrings used to score and select representative ADM lines
// for a parser-design sample. They are matched case-insensitively against the
// REAL file content; nothing here assumes a specific DayZ wording.
var gameplayTerms = []string{
	"kill", "hit", "died", "dead", "death", "damage", "connect", "disconnect",
	"player", "unconscious", "bleed", "shot", "spawn", "respawn", "position", "pos",
}

// ipLike matches IPv4 addresses for redaction only (network data, not gameplay).
var ipLike = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)

// SelectSampleLines picks up to max representative lines from ADM content using
// gameplay terms that actually appear in the file. Lines are returned verbatim
// (only IPs are redacted); no parsing or normalization happens here.
func SelectSampleLines(content string, max int) []string {
	if max <= 0 {
		max = 45
	}
	lines := strings.Split(content, "\n")

	type scored struct {
		idx   int
		text  string
		score int
	}
	picked := make([]scored, 0, len(lines))
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lower := strings.ToLower(line)
		score := 0
		for _, term := range gameplayTerms {
			if strings.Contains(lower, term) {
				score++
			}
		}
		if score > 0 {
			picked = append(picked, scored{idx: i, text: line, score: score})
		}
	}

	// If nothing matched gameplay terms, fall back to a spread of real lines so we
	// still see the file's actual structure.
	if len(picked) == 0 {
		step := 1
		if len(lines) > max {
			step = len(lines) / max
		}
		out := make([]string, 0, max)
		for i := 0; i < len(lines) && len(out) < max; i += step {
			line := strings.TrimRight(lines[i], "\r")
			if strings.TrimSpace(line) != "" {
				out = append(out, redactNetwork(line))
			}
		}
		return out
	}

	// Keep chronological order but cap to max, favoring higher-scoring lines by
	// sampling evenly across the matched set.
	step := 1
	if len(picked) > max {
		step = len(picked) / max
		if step < 1 {
			step = 1
		}
	}
	out := make([]string, 0, max)
	for i := 0; i < len(picked) && len(out) < max; i += step {
		out = append(out, redactNetwork(picked[i].text))
	}
	return out
}

// redactNetwork replaces IP-like network identifiers only; gameplay text is untouched.
func redactNetwork(line string) string {
	return ipLike.ReplaceAllString(line, "[REDACTED]")
}
