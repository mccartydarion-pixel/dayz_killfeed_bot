package shop

import (
	"encoding/base64"
	"strconv"
	"strings"
)

func encodeCursor(prefix string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(prefix + strconv.FormatInt(id, 10)))
}

func decodeCursor(prefix, s string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(string(raw), prefix), 10, 64)
	return id, err == nil && id > 0
}
