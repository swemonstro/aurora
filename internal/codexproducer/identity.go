package codexproducer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// instanceIDSalt scopes this package's instance-id hash away from any other
// hash built over similar inputs elsewhere in the codebase (defense in depth;
// the hash is never treated as a shared namespace with anything else).
const instanceIDSalt = "aurora-codexproducer-instance-id-v1"

// DeriveInstanceID returns a stable, content-free, opaque instance_id for one
// observed Codex OS process generation. It is a deterministic function of
// only the configured source label and the process's own generation-safe
// identity (PID + start time) — never cwd, argv, session id, prompt, or
// CODEX_HOME path — so:
//
//   - the same still-running Codex process always re-derives the same
//     instance_id, including across this producer's own restarts (only
//     ProducerEpoch changes on restart, not per-instance identity), which is
//     what lets a reconnecting producer resume the same instance's lease
//     under a new epoch;
//   - two processes started under different configured sources (e.g.
//     Business vs API) can never collide, even if the underlying PID and
//     start time were ever identical (impossible on one host, but the source
//     label is still mixed into the hash as defense in depth);
//   - PID reuse cannot collide two different instances, because start time is
//     always part of the identity (see internal/runtimerecognition, which
//     this package relies on to verify PID+StartedAt is a genuine, unreused
//     generation before this function is ever called).
func DeriveInstanceID(source SourceLabel, pid uint64, startedAt time.Time) producerprotocol.InstanceID {
	digestInput := fmt.Sprintf("%s\x00%s\x00%d\x00%d", instanceIDSalt, source, pid, startedAt.UnixNano())
	digest := sha256.Sum256([]byte(digestInput))
	return producerprotocol.InstanceID("codex-" + hex.EncodeToString(digest[:16]))
}

// NewProducerEpoch returns a fresh, opaque producer_epoch for one producer
// process generation: random, content-free, and stable for that process's
// entire lifetime. It carries no hostname, PID, CODEX_HOME, cwd, or session
// content, satisfying producerprotocol's opaque-identifier contract.
func NewProducerEpoch() (producerprotocol.ProducerEpoch, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate producer epoch: %w", err)
	}
	return producerprotocol.ProducerEpoch("codex-epoch-" + hex.EncodeToString(buffer)), nil
}
