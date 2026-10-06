package maprotation

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

var (
	ErrGameplayInvalid   = errors.New("cfggameplay.json is not valid JSON")
	ErrGameplayStructure = errors.New("cfggameplay.json does not have the expected structure (a WorldsData object whose objectSpawnersArr, if present, is a list of file names)")
	errEditNotMinimal    = errors.New("internal: the edit changed something other than objectSpawnersArr")
)

// GameplayEdit is the result of EditSpawners.
type GameplayEdit struct {
	Out     []byte   // the new file; the input itself when Changed is false
	Before  []string // objectSpawnersArr before
	After   []string // objectSpawnersArr after
	Changed bool
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// SpawnerEntry is how a map file is referenced in objectSpawnersArr.
func SpawnerEntry(mapFile string) string { return CustomDir + "/" + mapFile }

func normalizeEntry(e string) string {
	e = strings.ReplaceAll(strings.TrimSpace(e), "\\", "/")
	for strings.HasPrefix(e, "./") {
		e = e[2:]
	}
	return e
}

// EditSpawners makes add the one map file of this rotation in WorldsData.objectSpawnersArr.
//
// owned are the map file names of the installation's configured maps (base names). Every entry
// that references one of them (custom/<name>, compared without regard to case) is removed and
// custom/<add> takes the place of the first of them (or is appended). Every other entry, every
// other key and every other byte of the file is kept exactly: only the text of the array value
// changes. A WorldsData without objectSpawnersArr gets the key. A file that is not valid JSON, has
// no WorldsData object or whose objectSpawnersArr is not a list of strings is refused.
func EditSpawners(current []byte, owned []string, add string) (GameplayEdit, error) {
	if err := ValidateMapFile(add); err != nil {
		return GameplayEdit{}, err
	}
	return editSpawners(current, owned, add)
}

// EditChampionSpawner is EditSpawners for one of Champion's own reserved files (IsReservedName):
// it adds custom/<name> to objectSpawnersArr, once, and changes nothing else. An owner-supplied
// name is refused here just as a reserved name is refused by EditSpawners.
func EditChampionSpawner(current []byte, name string) (GameplayEdit, error) {
	if !IsReservedName(name) {
		return GameplayEdit{}, ErrFileName
	}
	return editSpawners(current, nil, name)
}

func editSpawners(current []byte, owned []string, add string) (GameplayEdit, error) {
	var ed GameplayEdit
	body := current
	if bytes.HasPrefix(body, utf8BOM) {
		body = body[len(utf8BOM):]
	}
	prefix := current[:len(current)-len(body)]
	if !json.Valid(body) {
		return ed, ErrGameplayInvalid
	}
	root := skipSpace(body, 0)
	if root >= len(body) || body[root] != '{' {
		return ed, ErrGameplayStructure
	}
	wd, found := findMember(body, root, "WorldsData")
	if !found || body[wd.valStart] != '{' {
		return ed, ErrGameplayStructure
	}
	ownedSet := map[string]bool{strings.ToLower(SpawnerEntry(add)): true}
	for _, name := range owned {
		if ValidateMapFile(name) == nil {
			ownedSet[strings.ToLower(SpawnerEntry(name))] = true
		}
	}
	entry := SpawnerEntry(add)

	arr, hasArr := findMember(body, wd.valStart, "objectSpawnersArr")
	var out []byte
	if hasArr {
		if body[arr.valStart] != '[' {
			return ed, ErrGameplayStructure
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(body[arr.valStart:arr.valEnd], &raw); err != nil {
			return ed, ErrGameplayStructure
		}
		before := make([]string, 0, len(raw))
		for _, r := range raw {
			var s string
			if len(r) == 0 || r[0] != '"' || json.Unmarshal(r, &s) != nil {
				return ed, ErrGameplayStructure // a number, null or nested value is not a file name
			}
			before = append(before, s)
		}
		after := make([]string, 0, len(before)+1)
		placed := false
		for _, e := range before {
			if !ownedSet[strings.ToLower(normalizeEntry(e))] {
				after = append(after, e)
				continue
			}
			if !placed {
				after = append(after, entry)
				placed = true
			}
		}
		if !placed {
			after = append(after, entry)
		}
		ed.Before, ed.After = before, after
		if reflect.DeepEqual(before, after) {
			ed.Out = current
			return ed, nil
		}
		rendered := renderArray(body, arr, after)
		out = splice(body, arr.valStart, arr.valEnd, rendered)
	} else {
		ed.Before, ed.After = []string{}, []string{entry}
		q, _ := json.Marshal(entry)
		member := `"objectSpawnersArr": [` + string(q) + `]`
		open := wd.valStart + 1
		first := skipSpace(body, open)
		if body[first] == '}' {
			out = splice(body, open, first, []byte(member))
		} else {
			lead := body[open:first] // the whitespace before the first existing key
			out = splice(body, open, open, append(append([]byte{}, lead...), []byte(member+",")...))
		}
	}
	if err := verifyEdit(body, out, ed.After); err != nil {
		return GameplayEdit{}, err
	}
	ed.Out, ed.Changed = append(append([]byte{}, prefix...), out...), true
	return ed, nil
}

func splice(b []byte, start, end int, with []byte) []byte {
	out := make([]byte, 0, len(b)+len(with))
	out = append(out, b[:start]...)
	out = append(out, with...)
	return append(out, b[end:]...)
}

// renderArray writes the entries in the style of the existing array: one per line with the
// original indentation when it spanned lines, otherwise on one line.
func renderArray(b []byte, arr member, entries []string) []byte {
	old := b[arr.valStart:arr.valEnd]
	quoted := make([]string, len(entries))
	for i, e := range entries {
		q, _ := json.Marshal(e)
		quoted[i] = string(q)
	}
	if !bytes.ContainsAny(old, "\n") {
		return []byte("[" + strings.Join(quoted, ", ") + "]")
	}
	nl := "\n"
	if bytes.Contains(old, []byte("\r\n")) {
		nl = "\r\n"
	}
	// Indentation of the key's line, of the closing bracket and of the elements.
	lineStart := bytes.LastIndexByte(b[:arr.keyStart], '\n') + 1
	keyIndent := string(b[lineStart:arr.keyStart])
	if strings.TrimLeft(keyIndent, " \t") != "" {
		keyIndent = ""
	}
	closeIndent := keyIndent
	if i := bytes.LastIndexByte(old, '\n'); i >= 0 {
		if ws := string(old[i+1 : len(old)-1]); strings.TrimLeft(ws, " \t") == "" {
			closeIndent = ws
		}
	}
	unit := "\t"
	if strings.HasPrefix(keyIndent, " ") {
		unit = "    "
	}
	elemIndent := closeIndent + unit
	if first := skipSpace(old, 1); first < len(old) && old[first] == '"' {
		if i := bytes.LastIndexByte(old[:first], '\n'); i >= 0 {
			elemIndent = string(old[i+1 : first])
		}
	}
	var sb strings.Builder
	sb.WriteString("[")
	for i, q := range quoted {
		sb.WriteString(nl + elemIndent + q)
		if i < len(quoted)-1 {
			sb.WriteString(",")
		}
	}
	sb.WriteString(nl + closeIndent + "]")
	return []byte(sb.String())
}

// verifyEdit proves the result parses, carries exactly the planned entries and equals the input in
// every other key and value.
func verifyEdit(before, after []byte, want []string) error {
	decode := func(b []byte) (map[string]any, error) {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var m map[string]any
		err := dec.Decode(&m)
		return m, err
	}
	cur, err := decode(before)
	if err != nil {
		return ErrGameplayInvalid
	}
	next, err := decode(after)
	if err != nil {
		return errEditNotMinimal
	}
	wdC, okC := cur["WorldsData"].(map[string]any)
	wdN, okN := next["WorldsData"].(map[string]any)
	if !okC || !okN {
		return ErrGameplayStructure
	}
	got, _ := json.Marshal(wdN["objectSpawnersArr"])
	exp, _ := json.Marshal(want)
	if !bytes.Equal(got, exp) {
		return errEditNotMinimal
	}
	delete(wdC, "objectSpawnersArr")
	delete(wdN, "objectSpawnersArr")
	if !reflect.DeepEqual(cur, next) {
		return errEditNotMinimal
	}
	return nil
}

// --- a minimal scanner over valid JSON -----------------------------------------------------------

type member struct{ keyStart, valStart, valEnd int }

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string starting at b[i] == '"'.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(b)
}

// skipValue returns the index after the JSON value starting at b[i].
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	}
	for i < len(b) && !strings.ContainsRune(",}] \t\r\n", rune(b[i])) {
		i++
	}
	return i
}

// findMember finds key among the direct members of the object starting at b[obj] == '{'. The input
// must already be valid JSON. With a duplicated key the last one wins, as it does for a JSON parser.
func findMember(b []byte, obj int, key string) (member, bool) {
	var hit member
	found := false
	i := skipSpace(b, obj+1)
	for i < len(b) && b[i] == '"' {
		keyStart := i
		i = skipString(b, i)
		var name string
		_ = json.Unmarshal(b[keyStart:i], &name)
		i = skipSpace(b, i)
		if i >= len(b) || b[i] != ':' {
			break
		}
		valStart := skipSpace(b, i+1)
		if valStart >= len(b) {
			break
		}
		valEnd := skipValue(b, valStart)
		if name == key {
			hit, found = member{keyStart, valStart, valEnd}, true
		}
		i = skipSpace(b, valEnd)
		if i < len(b) && b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
	return hit, found
}
