package livesync

import "github.com/yourname/dayz-killfeed/internal/nitrado"

// Tail reads: Live Sync reads only the bytes after its checkpoint once partial reads have been
// proven for the Nitrado service (nitrado/tail_trust.go; docs/NITRADO_POLLING.md). A tail read is
// never used for a source's first read, when the checkpoint is 0, or when the listing says the
// file is now smaller than the checkpoint (replaced or truncated).

// TailReader is the optional partial-read surface (*nitrado.Client implements it).
type TailReader = nitrado.TailReader
