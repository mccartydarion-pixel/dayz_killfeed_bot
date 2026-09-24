// Command nitrado-delta-probe is a read-only diagnostic tool (Champion Performance Phase 1.5,
// docs/NITRADO_DELTA_READS.md) that checks whether a live Nitrado service supports the seek,
// offset/count or HTTP Range partial-read mechanisms, and validates each one byte-for-byte against
// a full download of the same file segment before an operator enables NITRADO_DELTA_READ_MODE for
// that service.
//
// It performs GET/download requests only - it never writes, restarts, or reconfigures anything, and
// it never prints the Nitrado token or any signed URL. It is not run automatically anywhere; an
// operator runs it explicitly, with real credentials, against a service they choose to test:
//
//	NITRADO_TOKEN=... go run ./cmd/nitrado-delta-probe -service <serviceID>
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func run() error {
	serviceID := flag.String("service", "", "Nitrado service ID to probe (required)")
	filePath := flag.String("file", "", "specific ADM file path to probe (default: newest discovered ADM log)")
	probeLength := flag.Int64("length", 65536, "bytes to request per partial-read attempt")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout for the probe run")
	flag.Parse()

	if *serviceID == "" {
		return errors.New("-service is required")
	}
	token := os.Getenv("NITRADO_TOKEN")
	if token == "" {
		return errors.New("NITRADO_TOKEN environment variable is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := nitrado.NewClient(nitrado.DefaultBaseURL, token, nil)

	path := *filePath
	if path == "" {
		logs, err := client.ListLogs(ctx, *serviceID)
		if err != nil {
			return fmt.Errorf("list logs: %w", err)
		}
		if len(logs) == 0 {
			return errors.New("no ADM log files discovered for this service")
		}
		sort.Slice(logs, func(i, j int) bool { return logs[i].Modified.After(logs[j].Modified) })
		path = logs[0].Path
		fmt.Printf("discovered file: %s (size=%d modified=%s)\n", path, logs[0].Size, logs[0].Modified.UTC().Format(time.RFC3339))
	}

	fmt.Println("downloading full file as the byte-for-byte comparison baseline...")
	full, err := client.ReadLog(ctx, *serviceID, path)
	if err != nil {
		return fmt.Errorf("full read (baseline): %w", err)
	}
	fmt.Printf("baseline: %d bytes\n", len(full))

	offset, length := probeSegment(int64(len(full)), *probeLength)
	if length <= 0 {
		fmt.Println("file too small to probe a meaningful partial-read segment; nothing to test")
		return nil
	}
	fmt.Printf("probe segment: offset=%d length=%d\n\n", offset, length)

	modes := []nitrado.DeltaMode{nitrado.DeltaModeSeek, nitrado.DeltaModeOffsetQuery, nitrado.DeltaModeRange}
	mismatch := false
	for _, mode := range modes {
		result, ok := client.ReadLogFrom(ctx, *serviceID, path, offset, mode)
		if !ok {
			fmt.Printf("%-14s UNSUPPORTED\n", mode)
			continue
		}
		want := full[result.StartOffset:min64(result.StartOffset+int64(len(result.Data)), int64(len(full)))]
		if !bytes.Equal(result.Data, want) {
			mismatch = true
			fmt.Printf("%-14s SUPPORTED but BYTE MISMATCH (got %d bytes, %d did not match the full-read baseline) - DO NOT ENABLE for this service\n", mode, len(result.Data), countMismatch(result.Data, want))
			continue
		}
		fmt.Printf("%-14s SUPPORTED, byte-for-byte validated (%d bytes, method=%s)\n", mode, len(result.Data), result.Method)
	}

	fmt.Println()
	if mismatch {
		fmt.Println("RESULT: at least one mechanism returned data that did NOT match the full-read baseline. Do not enable NITRADO_DELTA_READ_MODE for this service until this is understood.")
		os.Exit(1)
	}
	fmt.Println("RESULT: every mechanism that reported SUPPORTED was validated byte-for-byte against the full-read baseline.")
	return nil
}

// probeSegment picks a bounded, mid-file segment to probe - not right at byte 0 (uninteresting) and
// not assuming the file is large.
func probeSegment(size, requested int64) (offset, length int64) {
	if size <= 0 {
		return 0, 0
	}
	length = requested
	// Never probe from byte 0: a server that ignores the offset returns the file from the start,
	// which is byte-identical to an offset-0 segment and would be a false "SUPPORTED" (observed on a
	// 124-byte Champions ADM, 2026-09-24). The segment is at most the file's second half.
	if length > size/2 {
		length = size / 2
	}
	offset = size / 2
	if offset+length > size {
		offset = size - length
	}
	if offset < 0 {
		offset = 0
	}
	return offset, length
}

func countMismatch(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	c := 0
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			c++
		}
	}
	c += abs(len(a) - len(b))
	return c
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
