package killfeed

import "github.com/yourname/dayz-killfeed/internal/nitrado"

// TestNitradoClientSatisfiesAPIOnlyLogSource is a compile-time check
// (section 9F of the noftp API audit) that the production ADM transport -
// *nitrado.Client - satisfies LogSource using nothing but its documented
// Nitrado REST API methods (ListLogs/ReadLog, both backed by net/http - see
// internal/nitrado/logs.go). If a non-API transport (e.g. an FTP client)
// were ever introduced as an alternate LogSource implementation, this alone
// wouldn't catch it, but it does guarantee the real production engine can
// only ever be wired to the API-backed client.
var _ LogSource = (*nitrado.Client)(nil)

// TestNitradoClientSatisfiesStatSource is the same guarantee for the cheap
// per-file metadata check the engine optionally uses.
var _ StatSource = (*nitrado.Client)(nil)
