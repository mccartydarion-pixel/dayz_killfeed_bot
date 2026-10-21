package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/livesync"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// --- server logs (docs/SERVER_LOGS.md) ---
//
// The server owner reads the game server's own log files - the ADM, the RPT, the script log and
// the crash log - straight from Nitrado. Champion keeps no copy. A request names a file, never a
// path: the name must be one Nitrado lists in the log directory right now, and the path read is
// the one from that listing.

const (
	// serverLogChunkBytes is one Nitrado seek read; serverLogWindowBytes is the most one request returns.
	serverLogChunkBytes  = 256 * 1024
	serverLogWindowBytes = 4 * serverLogChunkBytes
	// serverLogFullReadMax bounds the whole-file download used when Nitrado refuses a seek read.
	serverLogFullReadMax = 8 * 1024 * 1024
)

// serverLogSource is the part of the Nitrado client the log reader uses; *nitrado.Client satisfies it.
type serverLogSource interface {
	ConfigDir(ctx context.Context, serviceID string) (string, error)
	ListDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	ReadLogRange(ctx context.Context, serviceID, path string, offset, length int64) (*nitrado.PartialReadResult, bool)
	ReadRemoteFile(ctx context.Context, serviceID, path string) ([]byte, error)
}

var errServerLogTooLarge = errors.New("server log: Nitrado refused a partial read and the file is too large to download whole")

type serverLogFileDTO struct {
	Name string `json:"name"`
	// Kind is "adm", "rpt", "script" or "crash".
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	// ModifiedAt is when Nitrado last saw the file change (RFC 3339, UTC), "" when unknown.
	ModifiedAt string `json:"modifiedAt"`
	// StartedAt is the server-local start time in the file name ("2006-01-02 15:04:05"), "" when
	// the name carries none. It groups the files of one server start.
	StartedAt string `json:"startedAt"`
}

type serverLogListDTO struct {
	Files []serverLogFileDTO `json:"files"`
}

type serverLogContentDTO struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	// Start and End are the byte range of Text in the file. Passing Start back as `before` reads
	// the part above it.
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	HasEarlier bool   `json:"hasEarlier"`
	Text       string `json:"text"`
}

// serverLogKind is the kind shown for a file name, "" for a file that is not a server log.
func serverLogKind(name string) string {
	if strings.ContainsAny(name, `/\`) {
		return ""
	}
	switch livesync.ClassifySource(name).Family {
	case livesync.FamilyADM:
		return "adm"
	case livesync.FamilyRPT:
		return "rpt"
	case livesync.FamilyScript:
		return "script"
	case livesync.FamilyCrash:
		return "crash"
	}
	return ""
}

// listServerLogs returns the server logs Nitrado lists, newest server start first.
func listServerLogs(ctx context.Context, src serverLogSource, serviceID string) ([]nitrado.LogFile, error) {
	dir, err := src.ConfigDir(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	all, err := src.ListDir(ctx, serviceID, dir)
	if err != nil {
		return nil, err
	}
	files := make([]nitrado.LogFile, 0, len(all))
	for _, f := range all {
		if serverLogKind(f.Name) != "" && f.Path != "" {
			files = append(files, f)
		}
	}
	stamp := func(f nitrado.LogFile) time.Time {
		if t := livesync.ClassifySource(f.Name).FileLocalStart; t != nil {
			return *t
		}
		return time.Time{}
	}
	sort.SliceStable(files, func(i, j int) bool {
		si, sj := stamp(files[i]), stamp(files[j])
		if !si.Equal(sj) {
			return si.After(sj)
		}
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func serverLogFileOf(f nitrado.LogFile) serverLogFileDTO {
	dto := serverLogFileDTO{Name: f.Name, Kind: serverLogKind(f.Name), Size: f.Size}
	if !f.Modified.IsZero() {
		dto.ModifiedAt = f.Modified.UTC().Format(time.RFC3339)
	}
	if t := livesync.ClassifySource(f.Name).FileLocalStart; t != nil {
		dto.StartedAt = t.Format("2006-01-02 15:04:05")
	}
	return dto
}

// readServerLogWindow reads the part of file that ends at before (the end of the file when before
// is not inside it), at most serverLogWindowBytes long, cut to whole lines and redacted.
func readServerLogWindow(ctx context.Context, src serverLogSource, serviceID string, file nitrado.LogFile, before int64) (serverLogContentDTO, error) {
	out := serverLogContentDTO{Name: file.Name, Kind: serverLogKind(file.Name), Size: file.Size}
	if file.Size <= 0 {
		return out, nil
	}
	if before <= 0 || before > file.Size {
		before = file.Size
	}
	start := before - serverLogWindowBytes
	if start < 0 {
		start = 0
	}
	data, err := readServerLogBytes(ctx, src, serviceID, file, start, before)
	if err != nil {
		return out, err
	}
	// A window that starts inside the file starts inside a line: drop that partial line.
	if start > 0 {
		cut := strings.IndexByte(string(data), '\n')
		if cut < 0 {
			cut = len(data) - 1
		}
		start += int64(cut + 1)
		data = data[cut+1:]
	}
	out.Start, out.End, out.HasEarlier = start, before, start > 0
	out.Text = redactServerLog(string(data))
	return out, nil
}

// readServerLogBytes returns bytes [start, end) of file: Nitrado seek reads one chunk at a time,
// else the whole file when it is small enough.
func readServerLogBytes(ctx context.Context, src serverLogSource, serviceID string, file nitrado.LogFile, start, end int64) ([]byte, error) {
	data := make([]byte, 0, end-start)
	offset := start
	for offset < end {
		length := end - offset
		if length > serverLogChunkBytes {
			length = serverLogChunkBytes
		}
		res, ok := src.ReadLogRange(ctx, serviceID, file.Path, offset, length)
		if !ok || res == nil || res.StartOffset != offset || len(res.Data) == 0 {
			break
		}
		chunk := res.Data
		if int64(len(chunk)) > length {
			chunk = chunk[:length]
		}
		data = append(data, chunk...)
		offset += int64(len(chunk))
	}
	if offset >= end {
		return data, nil
	}
	if file.Size > serverLogFullReadMax {
		return nil, errServerLogTooLarge
	}
	whole, err := src.ReadRemoteFile(ctx, serviceID, file.Path)
	if err != nil {
		return nil, err
	}
	if int64(len(whole)) < end {
		end = int64(len(whole))
	}
	if start > end {
		start = end
	}
	return whole[start:end], nil
}

var (
	serverLogIPRe = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	// serverLogServiceRe is the Nitrado service account name, which appears in file paths.
	serverLogServiceRe = regexp.MustCompile(`\bni\d{4,}_\d+\w*`)
	serverLogSecretRe  = regexp.MustCompile(`(?i)\b(password\w*|passwd|secret|token|api_?key)(\s*[=:]\s*)("[^"]*"|\S+)`)
)

// redactServerLog hides what the page has no reason to show: the start-up command line and the
// crash log's "CLI params" (address, port, config), the Nitrado service account name, IP addresses
// and anything written as a password or token. Player names, ids and positions stay - they are what the logs are read for.
func redactServerLog(text string) string {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "?"), "\x00", "")
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "==") && strings.Trim(trimmed, "= ") != "" {
			lines[i] = "== [start-up command line hidden]"
			continue
		}
		// The crash log repeats the start-up parameters (address, port, config file) in every block.
		if i := strings.Index(line, "CLI params:"); i >= 0 {
			line = line[:i] + "CLI params: [hidden]"
		}
		line = serverLogServiceRe.ReplaceAllString(line, "[service hidden]")
		line = serverLogIPRe.ReplaceAllStringFunc(line, func(m string) string {
			for _, part := range strings.Split(m, ".") {
				if n, err := strconv.Atoi(part); err != nil || n > 255 {
					return m // a version number, not an address
				}
			}
			return "[ip hidden]"
		})
		lines[i] = serverLogSecretRe.ReplaceAllString(line, "$1$2[hidden]")
	}
	return strings.Join(lines, "\n")
}

func (a *App) handleListServerLogs(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerLogsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	files, err := listServerLogs(ctx, client, serviceID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "server log list failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not list the server's log files from Nitrado")
		return
	}
	out := serverLogListDTO{Files: make([]serverLogFileDTO, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, serverLogFileOf(f))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handleReadServerLog(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerLogsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("file"))
	if serverLogKind(name) == "" {
		writeSaaSError(w, codeInvalidRequest, "file must be the name of a server log")
		return
	}
	var before int64
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			writeSaaSError(w, codeInvalidRequest, "before must be a positive byte offset")
			return
		}
		before = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	files, err := listServerLogs(ctx, client, serviceID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "server log list failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not list the server's log files from Nitrado")
		return
	}
	var file *nitrado.LogFile
	for i := range files {
		if files[i].Name == name {
			file = &files[i]
			break
		}
	}
	if file == nil {
		writeSaaSError(w, codeNotFound, "that log file is no longer on the server")
		return
	}
	content, err := readServerLogWindow(ctx, client, serviceID, *file, before)
	if errors.Is(err, errServerLogTooLarge) {
		writeSaaSError(w, codeNitradoUnavailable, "Nitrado would not serve part of this file and it is too large to read whole")
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "msg", "server log read failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not read the log file from Nitrado")
		return
	}
	// One audit row per file opened; reading further up the same file adds none.
	if before == 0 {
		a.recordAudit(ctx, ac, "SERVER_LOG_VIEWED", fmt.Sprintf("server_log:%s", name), "", "success", nil, map[string]any{"bytes": content.End - content.Start})
	}
	writeSaaSJSON(w, http.StatusOK, content)
}
