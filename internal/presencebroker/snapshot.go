package presencebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
)

// DefaultSnapshotInterval is the maximum age of a written snapshot while the server runs.
const DefaultSnapshotInterval = 250 * time.Millisecond

// SnapshotSource is the registry surface used by the read-only snapshot writer.
type SnapshotSource interface {
	CanonicalSnapshot() (presencev2.CanonicalSnapshot, error)
}

// WriteCanonicalSnapshotAtomic marshals a CanonicalSnapshot and replaces path
// via temp file + fsync + rename. The final file mode is 0600.
func WriteCanonicalSnapshotAtomic(path string, source SnapshotSource) error {
	if source == nil {
		return fmt.Errorf("snapshot source must not be nil")
	}
	snapshot, err := source.CanonicalSnapshot()
	if err != nil {
		return fmt.Errorf("canonical snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("canonical snapshot invalid: %w", err)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	return writeFileAtomic(path, payload, 0o600)
}

// writeFileAtomic writes data to path with the given mode using a same-directory
// temporary file, fsync, and rename so readers never observe partial JSON.
//
// Every error returned here is passed through wrapOpaque: os.CreateTemp,
// File.Chmod/Write/Sync/Close, and os.Rename all return errors that embed a
// filesystem path (*fs.PathError or *os.LinkError) — the snapshot
// destination and/or a generated temp file name — and this function's
// errors ultimately reach RunSnapshotLoop's stderr log line, which must
// never leak either. errors.Is/Unwrap still work through wrapOpaque, so
// callers that need to inspect the underlying cause still can.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".aurora-snapshot-*.tmp")
	if err != nil {
		return wrapOpaque("create snapshot temp file", err)
	}
	tempName := temp.Name()
	success := false
	defer func() {
		if !success {
			_ = temp.Close()
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return wrapOpaque("set snapshot temp permissions", err)
	}
	if _, err := temp.Write(data); err != nil {
		return wrapOpaque("write snapshot temp file", err)
	}
	if err := temp.Sync(); err != nil {
		return wrapOpaque("fsync snapshot temp file", err)
	}
	if err := temp.Close(); err != nil {
		return wrapOpaque("close snapshot temp file", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return wrapOpaque("rename snapshot file", err)
	}
	success = true
	// Best-effort directory fsync so the rename is durable on supporting
	// filesystems; errors are deliberately discarded, never logged, so
	// there is nothing here that could leak a path either.
	if dir, err := os.Open(directory); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// RunSnapshotLoop writes snapshots on interval until ctx is cancelled.
// Write failures are logged to stderr and never stop the loop or other server work.
func RunSnapshotLoop(
	ctx context.Context,
	source SnapshotSource,
	path string,
	interval time.Duration,
	stderr io.Writer,
) {
	if interval <= 0 {
		interval = DefaultSnapshotInterval
	}
	// Immediate first write so consumers do not wait a full interval.
	if err := WriteCanonicalSnapshotAtomic(path, source); err != nil {
		fmt.Fprintln(stderr, "snapshot write:", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := WriteCanonicalSnapshotAtomic(path, source); err != nil {
				fmt.Fprintln(stderr, "snapshot write:", err)
			}
		}
	}
}

// ensure registry satisfies SnapshotSource.
var _ SnapshotSource = (*instanceregistry.Registry)(nil)
