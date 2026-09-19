package embedtemplates

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ValidationError lists every problem found, as fixed safe strings ("path:
// problem"). It never echoes free-form user text beyond a matched variable name.
type ValidationError struct{ Issues []string }

func (e *ValidationError) Error() string {
	return "invalid embed template: " + strings.Join(e.Issues, "; ")
}

var (
	// One placeholder: {{name}} with optional inner whitespace (the website's
	// interpolation accepts it); canonicalized to {{name}} on save.
	tokenRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
	colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// variableOwners maps every known variable to the routes that offer it (used only to
// tell "unknown" from "not available on this route").
var variableOwners = func() map[string]bool {
	out := map[string]bool{}
	for _, vars := range routeVariables {
		for _, v := range vars {
			out[v] = true
		}
	}
	return out
}()

type validator struct {
	route   map[string]bool
	routeID string
	issues  []string
}

func (v *validator) add(path, problem string) { v.issues = append(v.issues, path+": "+problem) }

func runes(s string) int { return utf8.RuneCountInString(s) }

// text validates one template string and returns its canonical form. Rules:
//   - only {{name}} placeholders, and only names approved for this route;
//   - every other brace is reserved (so no "{{ .Field }}", "{{a | b}}", "{{f()}}",
//     "${x}", "{% ... %}" or a half-written "{{killer}" can get through);
//   - no control characters other than newline and tab.
func (v *validator) text(path, s string, max int, required bool) string {
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' && r != '\r' || r == 0x7f }) {
		v.add(path, "contains control characters")
	}
	canonical := tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		name := tokenRe.FindStringSubmatch(tok)[1]
		switch {
		case v.route[name]:
			return "{{" + name + "}}"
		case variableOwners[name]:
			v.add(path, fmt.Sprintf("variable {{%s}} is not available for %s", name, v.routeID))
		default:
			v.add(path, fmt.Sprintf("unknown variable {{%s}}", clip(name, 32)))
		}
		return tok
	})
	if rest := tokenRe.ReplaceAllString(canonical, ""); strings.ContainsAny(rest, "{}") {
		v.add(path, "braces are reserved for {{variable}} placeholders (malformed or unsupported placeholder)")
	}
	if n := runes(canonical); n > max {
		v.add(path, fmt.Sprintf("too long (%d, max %d)", n, max))
	}
	if required && strings.TrimSpace(canonical) == "" {
		v.add(path, "must not be empty when enabled")
	}
	return canonical
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// urlField validates an optional http(s) URL. Placeholders are not allowed in URLs.
func (v *validator) urlField(path, raw string, required bool) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		if required {
			v.add(path, "is required when enabled")
		}
		return ""
	}
	if len(s) > MaxURLLength {
		v.add(path, fmt.Sprintf("too long (max %d)", MaxURLLength))
		return s
	}
	if strings.ContainsFunc(s, func(r rune) bool { return r <= 0x20 || r == 0x7f }) || strings.Contains(s, "{{") {
		v.add(path, "must be a plain http(s) URL")
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		v.add(path, "must be a valid http(s) URL")
		return s
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		v.add(path, "must use http or https")
		return s
	}
	if u.User != nil {
		v.add(path, "must not contain credentials")
	}
	return s
}

// Validate checks cfg for routeKey and returns the normalized config to store:
// route key set from the URL, version set, color upper-cased, placeholders
// canonicalized, fields ordered and renumbered 0..n-1. It returns *ValidationError
// listing every issue when invalid.
func Validate(cfg Config, routeKey string) (Config, error) {
	vars, ok := routeVariables[routeKey]
	if !ok {
		return Config{}, &ValidationError{Issues: []string{"routeKey: unknown route"}}
	}
	v := &validator{route: map[string]bool{}, routeID: routeKey}
	for _, name := range vars {
		v.route[name] = true
	}
	out := cfg
	if cfg.RouteKey != "" && cfg.RouteKey != routeKey {
		v.add("routeKey", "does not match the route in the URL")
	}
	out.RouteKey = routeKey
	out.Version = SchemaVersion

	// Color: exactly #RRGGBB (the website's representation); never wrapped or clamped.
	if !colorRe.MatchString(cfg.Color) {
		v.add("color", "must be a #RRGGBB hex color")
	} else {
		out.Color = strings.ToUpper(cfg.Color)
	}

	total := 0
	count := func(enabled bool, s string) {
		if enabled {
			total += runes(s)
		}
	}

	out.Title.Template = v.text("title.template", cfg.Title.Template, MaxTitle, cfg.Title.Enabled)
	count(cfg.Title.Enabled, out.Title.Template)
	out.Description.Template = v.text("description.template", cfg.Description.Template, MaxDescription, cfg.Description.Enabled)
	count(cfg.Description.Enabled, out.Description.Template)

	out.Author.Name = v.text("author.name", cfg.Author.Name, MaxAuthorName, cfg.Author.Enabled)
	count(cfg.Author.Enabled, out.Author.Name)
	out.Author.IconURL = v.urlField("author.iconUrl", cfg.Author.IconURL, false)
	out.Thumbnail.URL = v.urlField("thumbnail.url", cfg.Thumbnail.URL, cfg.Thumbnail.Enabled)
	out.Image.URL = v.urlField("image.url", cfg.Image.URL, cfg.Image.Enabled)
	out.Footer.Text = v.text("footer.text", cfg.Footer.Text, MaxFooterText, cfg.Footer.Enabled)
	count(cfg.Footer.Enabled, out.Footer.Text)
	out.Footer.IconURL = v.urlField("footer.iconUrl", cfg.Footer.IconURL, false)

	if len(cfg.Fields) > MaxFields {
		v.add("fields", fmt.Sprintf("too many fields (%d, max %d)", len(cfg.Fields), MaxFields))
	}
	seen := map[string]bool{}
	fields := make([]Field, 0, len(cfg.Fields))
	for i, f := range cfg.Fields {
		if i >= MaxFields { // report the count once; do not validate the excess
			break
		}
		p := fmt.Sprintf("fields[%d]", i)
		nf := f
		if !keyRe.MatchString(f.Key) {
			v.add(p+".key", "must be 1-64 letters, digits, '_' or '-'")
		} else if lk := strings.ToLower(f.Key); seen[lk] {
			v.add(p+".key", "duplicate field key")
		} else {
			seen[lk] = true
		}
		if f.Order < 0 || f.Order > 10000 {
			v.add(p+".order", "must be between 0 and 10000")
		}
		nf.Label = v.text(p+".label", f.Label, MaxFieldLabel, f.Enabled)
		nf.Template = v.text(p+".template", f.Template, MaxFieldValue, f.Enabled)
		count(f.Enabled, nf.Label)
		count(f.Enabled, nf.Template)
		fields = append(fields, nf)
	}
	sort.SliceStable(fields, func(a, b int) bool { return fields[a].Order < fields[b].Order })
	for i := range fields {
		fields[i].Order = i
	}
	out.Fields = fields

	if total > MaxTotalText {
		v.add("total", fmt.Sprintf("combined embed text too long (%d, max %d)", total, MaxTotalText))
	}
	if cfg.Enabled && !anyContent(cfg) {
		v.add("template", "an enabled embed needs at least one enabled title, description, field, author, footer or image")
	}

	if len(v.issues) > 0 {
		return Config{}, &ValidationError{Issues: v.issues}
	}
	return out, nil
}

func anyContent(c Config) bool {
	if c.Title.Enabled || c.Description.Enabled || c.Author.Enabled || c.Footer.Enabled || c.Image.Enabled || c.Thumbnail.Enabled {
		return true
	}
	for _, f := range c.Fields {
		if f.Enabled {
			return true
		}
	}
	return false
}
