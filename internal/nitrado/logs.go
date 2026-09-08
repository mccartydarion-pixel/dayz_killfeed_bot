package nitrado

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogFile represents a discovered server file candidate.
type LogFile struct {
	Name      string
	Path      string
	Directory string
	Size      int64
	Modified  time.Time
	Type      string
	Source    string
}

// inspectWatchKeys are field names inspected when scanning a service payload
// for file/log access capabilities.
var inspectWatchKeys = []string{
	"file", "files", "log", "logs", "ftp", "sftp", "server", "gameserver",
	"webinterface", "directory", "path", "host", "port", "username", "credentials",
}

// sensitiveKeyMarkers identify values that must never be logged.
var sensitiveKeyMarkers = []string{"password", "passwd", "token", "secret", "credential", "auth", "apikey", "api_key", "private"}

// IsSensitiveKey reports whether a field name indicates a credential or secret.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, marker := range sensitiveKeyMarkers {
		if strings.Contains(k, marker) {
			return true
		}
	}
	return false
}

// InspectService recursively scans the configured service payload for file/log
// access fields and logs sanitized candidates. Sensitive values are redacted.
func (c *Client) InspectService(ctx context.Context, serviceID string) error {
	payload, err := c.servicePayload(ctx, serviceID)
	if err != nil {
		return err
	}

	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return fmt.Errorf("decode service payload for inspection: %w", err)
	}

	matches := 0
	walkServiceFields("", root, func(path string, value any) {
		matches++
		logInspectedField(path, value)
	})
	if matches == 0 {
		slog.Info("component=nitrado", "msg", "no file/log capability fields discovered in service payload", "service_id", serviceID)
	}
	return nil
}

// walkServiceFields recursively visits payload fields and invokes fn for every
// watched key. fn receives the dotted path and the raw value.
func walkServiceFields(prefix string, value any, fn func(path string, value any)) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if isWatchKey(key) {
				fn(path, child)
			}
			walkServiceFields(path, child, fn)
		}
	case []any:
		for i, child := range v {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			walkServiceFields(path, child, fn)
		}
	}
}

func isWatchKey(key string) bool {
	k := strings.ToLower(key)
	for _, watched := range inspectWatchKeys {
		if k == watched || strings.Contains(k, watched) {
			return true
		}
	}
	return false
}

func logInspectedField(path string, value any) {
	leaf := path
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		leaf = path[idx+1:]
	}
	if IsSensitiveKey(leaf) {
		slog.Info("component=nitrado", "path", path, "value", "<redacted>")
		return
	}
	switch v := value.(type) {
	case map[string]any, []any:
		slog.Info("component=nitrado", "path", path, "available", true)
	case string:
		slog.Info("component=nitrado", "path", path, "value", v)
	case nil:
		slog.Info("component=nitrado", "path", path, "available", false)
	default:
		slog.Info("component=nitrado", "path", path, "value", fmt.Sprintf("%v", v))
	}
}

// DiscoverLogs inspects the authenticated service response for file/log metadata.
func (c *Client) DiscoverLogs(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}

	_, err := c.ListLogs(ctx, serviceID)
	return err
}

// fileServerListEntry is one entry returned by the documented file server list API.
type fileServerListEntry struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
}

// logSearchDirs are the directory candidates most likely to contain DayZ logs,
// tried in order. The DayZ profile directory is profile/ on Nitrado.
var logSearchDirs = []string{"/", "/profile", "/profiles", "/dayz", "/config"}

// logNameMarkers identify DayZ gameplay/admin log files.
var logNameMarkers = []string{".rpt", ".adm", ".log"}

// ListLogs discovers real log files via the documented file server API
// (GET /services/:id/gameservers/file_server/list?dir=...). It no longer
// guesses paths from the service detail payload, which exposes none.
func (c *Client) ListLogs(ctx context.Context, serviceID string) ([]LogFile, error) {
	if serviceID == "" {
		return nil, fmt.Errorf("service ID is required")
	}

	found := make([]LogFile, 0)
	seen := map[string]struct{}{}
	permissionDenied := false

	for _, dir := range logSearchDirs {
		entries, err := c.listFileServerDir(ctx, serviceID, dir)
		if err != nil {
			var reqErr *RequestError
			if errors.As(err, &reqErr) {
				if reqErr.Kind == KindPermission {
					permissionDenied = true
				}
				if reqErr.Kind == KindAuthentication || reqErr.Kind == KindInvalidEndpoint {
					return nil, err
				}
			}
			continue
		}

		for _, e := range entries {
			// Descend one level into a directory that likely holds the DayZ profile.
			if e.Type == "dir" && isProfileDir(e.Name) {
				sub, err := c.listFileServerDir(ctx, serviceID, e.Path)
				if err == nil {
					entries = append(entries, sub...)
				}
				continue
			}
			if e.Type != "file" || !isLogFileName(e.Name) {
				continue
			}
			path := e.Path
			if path == "" {
				path = joinRemotePath(dir, e.Name)
			}
			if _, dup := seen[path]; dup {
				continue
			}
			seen[path] = struct{}{}
			found = append(found, LogFile{
				Name:      e.Name,
				Path:      path,
				Directory: dir,
				Size:      e.Size,
				Modified:  unixToTime(e.ModifiedAt),
				Type:      inferLogType(e.Name, path, "file_server"),
				Source:    "file_server",
			})
		}
	}

	if len(found) == 0 {
		if permissionDenied {
			return nil, &RequestError{Op: "log discovery", Kind: KindPermission, Message: "token lacks ROLE_WEBINTERFACE_FILEBROWSER_READ", StatusCode: http.StatusForbidden}
		}
		return nil, fmt.Errorf("no log files discovered for service %s", serviceID)
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].Modified.Equal(found[j].Modified) {
			return found[i].Name < found[j].Name
		}
		return found[i].Modified.After(found[j].Modified)
	})

	return found, nil
}

// listFileServerDir lists one directory via the documented file server list endpoint.
func (c *Client) listFileServerDir(ctx context.Context, serviceID, dir string) ([]fileServerListEntry, error) {
	endpoint := "/services/" + url.PathEscape(serviceID) + "/gameservers/file_server/list"
	if dir != "" && dir != "/" {
		endpoint += "?dir=" + url.QueryEscape(dir)
	}

	resp, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus("file server list", resp.StatusCode, KindNotFound)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file server list: %w", err)
	}

	var envelope struct {
		Data struct {
			Entries []fileServerListEntry `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode file server list: %w", err)
	}
	return envelope.Data.Entries, nil
}

func isProfileDir(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "profile" || n == "profiles" || n == "config" || n == "dayz"
}

func isLogFileName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, marker := range logNameMarkers {
		if strings.HasSuffix(n, marker) {
			return true
		}
	}
	return false
}

func joinRemotePath(dir, name string) string {
	if dir == "" || dir == "/" {
		return "/" + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

func unixToTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// ReadLog reads a discovered log source when the service exposes a direct URL or readable path.
func (c *Client) ReadLog(ctx context.Context, serviceID string, path string) ([]byte, error) {
	if serviceID == "" {
		return nil, fmt.Errorf("service ID is required")
	}
	if path == "" {
		return nil, fmt.Errorf("log path is required")
	}

	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		resp, err := c.httpClient.Get(path)
		if err != nil {
			return nil, fmt.Errorf("read remote log: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, &RequestError{Op: "read remote log", Kind: KindUnknown, Message: fmt.Sprintf("status=%d", resp.StatusCode), StatusCode: resp.StatusCode}
		}
		return io.ReadAll(resp.Body)
	}

	if _, err := os.Stat(path); err == nil {
		return os.ReadFile(path)
	}

	return nil, fmt.Errorf("log path must be a discoverable URL or local file; no reliable log source was identified for service %s", serviceID)
}

func (c *Client) servicePayload(ctx context.Context, serviceID string) ([]byte, error) {
	endpoint := "/services/" + url.PathEscape(serviceID)
	resp, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// A 404 on a service-specific path means the service ID was not found,
	// which is a lookup failure — never an authentication failure.
	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus("service verification", resp.StatusCode, KindNotFound)
	}

	return io.ReadAll(resp.Body)
}

func parseLogCandidates(payload []byte) ([]LogFile, error) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("decode service payload: %w", err)
	}

	result := make([]LogFile, 0)
	keys := []string{"files", "file", "logs", "log", "game_logs", "gamelogs", "ftp", "sftp", "file_server", "fileserver", "directory", "directories", "server_files"}

	for _, key := range keys {
		if value, ok := obj[key]; ok {
			entries := extractLogEntries(value, key)
			result = append(result, entries...)
		}
	}

	if len(result) == 0 {
		for _, value := range obj {
			entries := extractLogEntries(value, "")
			result = append(result, entries...)
		}
	}

	seen := map[string]struct{}{}
	filtered := make([]LogFile, 0, len(result))
	for _, entry := range result {
		if entry.Name == "" && entry.Path == "" {
			continue
		}
		k := entry.Path + "|" + entry.Name
		if _, exists := seen[k]; exists {
			continue
		}
		seen[k] = struct{}{}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

func extractLogEntries(value any, key string) []LogFile {
	if value == nil {
		return nil
	}

	results := make([]LogFile, 0)

	switch v := value.(type) {
	case []any:
		for _, item := range v {
			results = append(results, extractLogEntries(item, key)...)
		}
	case map[string]any:
		name := firstNonEmpty(v["name"], v["filename"], v["file_name"], v["path"], v["directory"], v["title"])
		path := firstNonEmpty(v["path"], v["url"], v["href"], v["download"], v["link"], v["file_path"], v["full_path"], v["location"])
		directory := firstNonEmpty(v["directory"], v["folder"], v["path"], v["location"])
		size := parseInt64(v["size"], v["bytes"], v["length"])
		modified, _ := parseTime(v["modified"], v["updated"], v["timestamp"], v["mtime"])
		logType := inferLogType(asString(name), asString(path), key)
		if name != "" || path != "" {
			results = append(results, LogFile{
				Name:      asString(name),
				Path:      asString(path),
				Directory: asString(directory),
				Size:      size,
				Modified:  modified,
				Type:      logType,
				Source:    key,
			})
		}
		for nestedKey, nestedValue := range v {
			if nestedKey == "name" || nestedKey == "path" || nestedKey == "url" || nestedKey == "directory" {
				continue
			}
			results = append(results, extractLogEntries(nestedValue, nestedKey)...)
		}
	case string:
		if strings.Contains(strings.ToLower(v), "log") || strings.Contains(strings.ToLower(v), ".log") || strings.Contains(strings.ToLower(v), "ftp") || strings.Contains(strings.ToLower(v), "sftp") {
			results = append(results, LogFile{Name: v, Path: v, Type: inferLogType(v, v, key)})
		}
	}

	return results
}

func firstNonEmpty(values ...any) any {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case float64:
			return v
		case int64:
			return v
		case int:
			return v
		case json.Number:
			return v
		}
	}
	return nil
}

func parseInt64(values ...any) int64 {
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			return int64(v)
		case int:
			return int64(v)
		case int64:
			return v
		case string:
			if n, err := strconvParseInt(v); err == nil {
				return n
			}
		}
	}
	return 0
}

func parseTime(values ...any) (time.Time, error) {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t, nil
			}
			if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
				return t, nil
			}
		case float64:
			return time.Unix(0, int64(v)*int64(time.Millisecond)), nil
		case int:
			return time.Unix(0, int64(v)*int64(time.Millisecond)), nil
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return time.Unix(0, n*int64(time.Millisecond)), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("no time value")
}

func inferLogType(name string, path string, key string) string {
	combined := strings.ToLower(strings.TrimSpace(name + " " + path + " " + key))
	switch {
	case strings.Contains(combined, "rpt"):
		return "RPT"
	case strings.Contains(combined, "adm"):
		return "ADM"
	case strings.Contains(combined, "console"):
		return "CONSOLE"
	case strings.Contains(combined, "script"):
		return "SCRIPT"
	case strings.Contains(combined, "game"):
		return "GAME"
	case strings.Contains(combined, "log"):
		return "LOG"
	case strings.Contains(combined, "ftp") || strings.Contains(combined, "sftp"):
		return "FILE_SERVER"
	default:
		return "FILE"
	}
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func strconvParseInt(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}
