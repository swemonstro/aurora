package localhooktransport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

const (
	DefaultIngestSequencingCapacity = 1024
	DefaultIngestReplayCapacity     = 4096
	DefaultIngestHandlingTime       = 5 * time.Second
	producerEpochEntropyBytes       = 16
)

// IngestServerConfig configures the Package 6 observe-only ingress receiver.
type IngestServerConfig struct {
	MaximumRequestBytes    uint32
	MaximumResponseBytes   uint32
	MaximumRequestIDLength int
	MaximumOpaqueLength    int
	MaximumHandlingTime    time.Duration
	SequencingCapacity     int
	ReplayCapacity         int
	ReplayTTL              time.Duration
	MaximumRevision        uint64
	Clock                  Clock
}

func DefaultIngestServerConfig(clock Clock) IngestServerConfig {
	return IngestServerConfig{
		MaximumRequestBytes:    DefaultIngestMaximumRequestBytes,
		MaximumResponseBytes:   DefaultIngestMaximumResponseBytes,
		MaximumRequestIDLength: 64,
		MaximumOpaqueLength:    128,
		MaximumHandlingTime:    DefaultIngestHandlingTime,
		SequencingCapacity:     DefaultIngestSequencingCapacity,
		ReplayCapacity:         DefaultIngestReplayCapacity,
		ReplayTTL:              5 * time.Minute,
		MaximumRevision:        1<<53 - 1,
		Clock:                  clock,
	}
}

func (config IngestServerConfig) Validate() error {
	if config.MaximumRequestBytes == 0 || config.MaximumRequestBytes > DefaultIngestMaximumRequestBytes {
		return errors.New("ingest maximum request bytes must be between 1 and 8192")
	}
	if config.MaximumResponseBytes == 0 || config.MaximumResponseBytes > DefaultIngestMaximumResponseBytes {
		return errors.New("ingest maximum response bytes must be between 1 and 4096")
	}
	if config.MaximumRequestIDLength < 8 || config.MaximumRequestIDLength > 128 ||
		config.MaximumOpaqueLength < 8 || config.MaximumOpaqueLength > 256 {
		return errors.New("ingest identifier limits are outside supported bounds")
	}
	if config.MaximumHandlingTime <= 0 {
		return errors.New("ingest handling time must be positive")
	}
	if config.SequencingCapacity < 1 || config.SequencingCapacity > DefaultIngestSequencingCapacity {
		return errors.New("sequencing capacity must be between 1 and 1024")
	}
	if config.ReplayCapacity < 1 || config.ReplayCapacity > DefaultIngestReplayCapacity {
		return errors.New("ingest replay capacity must be between 1 and 4096")
	}
	if config.ReplayTTL <= 0 {
		return errors.New("ingest replay TTL must be positive")
	}
	if config.MaximumRevision == 0 || config.MaximumRevision > 1<<53-1 {
		return errors.New("ingest maximum revision is outside the safe integer range")
	}
	if config.Clock == nil {
		return errors.New("clock must not be nil")
	}
	return nil
}

func (config IngestServerConfig) clientLimits() IngestClientConfig {
	return IngestClientConfig{
		MaximumRequestIDLength: config.MaximumRequestIDLength,
		MaximumOpaqueLength:    config.MaximumOpaqueLength,
	}
}

type ingestStreamKey struct {
	tool    instancepresence.ToolKind
	session instancepresence.OpaqueIdentity
}

type ingestStreamState struct {
	lastRevision uint64
	ended        bool
}

type ingestReplayState uint8

const (
	ingestReplayInProgress ingestReplayState = iota + 1
	ingestReplayCompleted
)

type ingestReplayEntry struct {
	digest    [sha256.Size]byte
	state     ingestReplayState
	response  IngestResponse
	expiresAt time.Time
}

// AcceptedIngress is the server-owned sequenced observation produced at the
// atomic accept point. It is kept only in memory and is never published.
type AcceptedIngress struct {
	Tool           instancepresence.ToolKind
	HookSessionRef instancepresence.OpaqueIdentity
	Lifecycle      instancecorrelation.Lifecycle
	ProducerEpoch  instancepresence.ProducerEpoch
	Revision       uint64
	ObservedAt     time.Time
}

// IngestReceiver is the Package 6 server-side sequencing owner. It never binds,
// mutates registry state, or performs correlation.
type IngestReceiver struct {
	config   IngestServerConfig
	epoch    instancepresence.ProducerEpoch
	mutex    sync.Mutex
	streams  map[ingestStreamKey]*ingestStreamState
	replay   map[string]ingestReplayEntry
	accepted map[ingestStreamKey]AcceptedIngress

	// afterAccept is an optional test hook invoked after the atomic accept
	// point while the request is still in_progress. It must not be used in
	// production composition.
	afterAccept func(context.Context)
}

func NewProducerEpoch() (instancepresence.ProducerEpoch, error) {
	var entropy [producerEpochEntropyBytes]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate producer epoch: %w", err)
	}
	epoch := instancepresence.ProducerEpoch(hex.EncodeToString(entropy[:]))
	if err := epoch.Validate(); err != nil {
		return "", err
	}
	return epoch, nil
}

func NewIngestReceiver(config IngestServerConfig) (*IngestReceiver, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	epoch, err := NewProducerEpoch()
	if err != nil {
		return nil, err
	}
	return &IngestReceiver{
		config:   config,
		epoch:    epoch,
		streams:  make(map[ingestStreamKey]*ingestStreamState),
		replay:   make(map[string]ingestReplayEntry),
		accepted: make(map[ingestStreamKey]AcceptedIngress),
	}, nil
}

func (receiver *IngestReceiver) ProducerEpoch() instancepresence.ProducerEpoch {
	return receiver.epoch
}

func (receiver *IngestReceiver) LastAccepted(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) (AcceptedIngress, bool) {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()
	value, ok := receiver.accepted[ingestStreamKey{tool: tool, session: session}]
	return value, ok
}

func (receiver *IngestReceiver) StreamRevision(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) (uint64, bool) {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()
	stream, ok := receiver.streams[ingestStreamKey{tool: tool, session: session}]
	if !ok {
		return 0, false
	}
	return stream.lastRevision, true
}

func (receiver *IngestReceiver) HandleJSON(ctx context.Context, data []byte) (response IngestResponse) {
	if uint64(len(data)) > uint64(receiver.config.MaximumRequestBytes) {
		return emptyIngestResponse(StatusRejected, "", CodeRequestTooLarge)
	}
	request, err := DecodeIngestRequestJSON(data)
	if err != nil {
		return emptyIngestResponse(StatusRejected, "", errorCode(err))
	}
	return receiver.Handle(ctx, request)
}

func (receiver *IngestReceiver) Handle(ctx context.Context, request IngestRequest) (response IngestResponse) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ValidateIngestRequest(receiver.config.clientLimits(), request); err != nil {
		requestID := ""
		if validateOpaque("request ID", request.RequestID, receiver.config.MaximumRequestIDLength, true) == nil {
			requestID = request.RequestID
		}
		return emptyIngestResponse(StatusRejected, requestID, errorCode(err))
	}

	// Establish the owning handler deadline before the reservation is visible.
	handleCtx, cancel := context.WithTimeout(ctx, receiver.config.MaximumHandlingTime)
	defer cancel()

	digest, err := canonicalIngressDigest(request.Payload)
	if err != nil {
		return emptyIngestResponse(StatusRejected, request.RequestID, CodeInternalError)
	}

	var (
		owned     bool
		accepted  AcceptedIngress
		finalized bool
	)
	defer func() {
		if recovered := recover(); recovered != nil {
			response = emptyIngestResponse(StatusError, request.RequestID, CodeInternalError)
		}
		if owned && !finalized {
			receiver.complete(request.RequestID, digest, response)
			finalized = true
		}
		response = ensureIngestNoBinding(response)
	}()

	outcome, accepted, owned := receiver.accept(request.RequestID, digest, request.Payload)
	if !owned {
		return outcome
	}

	if receiver.afterAccept != nil {
		receiver.afterAccept(handleCtx)
	}

	if err := handleCtx.Err(); err != nil {
		response = emptyIngestResponse(StatusError, request.RequestID, CodeHandlerTimeout)
		receiver.complete(request.RequestID, digest, response)
		finalized = true
		return response
	}

	// Observe-only Package 6 server path: sequencing is complete. Correlation,
	// binding, registry mutation, and publication are intentionally not performed.
	_ = accepted
	response = emptyIngestResponse(StatusOK, request.RequestID)
	receiver.complete(request.RequestID, digest, response)
	finalized = true
	return response
}

func (receiver *IngestReceiver) accept(requestID string, digest [sha256.Size]byte, payload IngressPayload) (IngestResponse, AcceptedIngress, bool) {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()

	now := canonicalTime(receiver.config.Clock.Now())
	receiver.pruneReplayLocked(now)

	if entry, exists := receiver.replay[requestID]; exists {
		if entry.digest != digest {
			return emptyIngestResponse(StatusRejected, requestID, CodeRequestIDConflict), AcceptedIngress{}, false
		}
		if entry.state == ingestReplayInProgress {
			return emptyIngestResponse(StatusDuplicate, requestID, CodeRequestInProgress), AcceptedIngress{}, false
		}
		// Completed identical replay: return cached final result with duplicate status.
		cached := entry.response
		cached.Status = StatusDuplicate
		cached.RequestID = requestID
		cached.ProtocolVersion = IngestProtocolVersion
		cached.NoBindingPerformed = true
		if cached.ErrorCodes == nil {
			cached.ErrorCodes = []ErrorCode{}
		}
		return cached, AcceptedIngress{}, false
	}

	if len(receiver.replay) >= receiver.config.ReplayCapacity {
		return emptyIngestResponse(StatusRejected, requestID, CodeReplayCacheFull), AcceptedIngress{}, false
	}

	key := ingestStreamKey{tool: payload.Tool, session: payload.HookSessionRef}
	stream, exists := receiver.streams[key]
	if !exists {
		if len(receiver.streams) >= receiver.config.SequencingCapacity {
			return emptyIngestResponse(StatusRejected, requestID, CodeSequencingCapacityExceeded), AcceptedIngress{}, false
		}
		stream = &ingestStreamState{}
		receiver.streams[key] = stream
	}
	if stream.lastRevision >= receiver.config.MaximumRevision {
		return emptyIngestResponse(StatusRejected, requestID, CodeRevisionOverflow), AcceptedIngress{}, false
	}

	revision := stream.lastRevision + 1
	observedAt := now
	accepted := AcceptedIngress{
		Tool:           payload.Tool,
		HookSessionRef: payload.HookSessionRef,
		Lifecycle:      payload.Lifecycle,
		ProducerEpoch:  receiver.epoch,
		Revision:       revision,
		ObservedAt:     observedAt,
	}
	if err := accepted.domain().Validate(); err != nil {
		return emptyIngestResponse(StatusError, requestID, CodeInternalError), AcceptedIngress{}, false
	}

	// Atomic accept: reserve replay ID, assign revision/time, publish stream state.
	receiver.replay[requestID] = ingestReplayEntry{
		digest: digest,
		state:  ingestReplayInProgress,
	}
	stream.lastRevision = revision
	if payload.Lifecycle == instancecorrelation.LifecycleEnded {
		stream.ended = true
	}
	receiver.accepted[key] = accepted
	return IngestResponse{}, accepted, true
}

func (receiver *IngestReceiver) complete(requestID string, digest [sha256.Size]byte, response IngestResponse) {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()

	now := canonicalTime(receiver.config.Clock.Now())
	response = ensureIngestNoBinding(response)
	response.ProtocolVersion = IngestProtocolVersion
	response.RequestID = requestID
	if response.ErrorCodes == nil {
		response.ErrorCodes = []ErrorCode{}
	}
	response.ErrorCodes = sortedErrorCodes(response.ErrorCodes)

	entry, exists := receiver.replay[requestID]
	if !exists {
		// Reservation should always exist for owned handlers; recreate conservatively.
		entry = ingestReplayEntry{digest: digest}
	}
	entry.digest = digest
	entry.state = ingestReplayCompleted
	entry.response = response
	entry.expiresAt = now.Add(receiver.config.ReplayTTL)
	receiver.replay[requestID] = entry
}

func (receiver *IngestReceiver) pruneReplayLocked(now time.Time) {
	for requestID, entry := range receiver.replay {
		if entry.state == ingestReplayCompleted && !now.Before(entry.expiresAt) {
			delete(receiver.replay, requestID)
		}
	}
}

func (accepted AcceptedIngress) domain() instancecorrelation.HookObservation {
	return instancecorrelation.HookObservation{
		Tool:           accepted.Tool,
		HookSessionRef: accepted.HookSessionRef,
		ProducerEpoch:  accepted.ProducerEpoch,
		Revision:       accepted.Revision,
		ObservedAt:     accepted.ObservedAt,
		Lifecycle:      accepted.Lifecycle,
	}
}

func canonicalIngressDigest(payload IngressPayload) ([sha256.Size]byte, error) {
	// Canonical body equality is over the validated triple only, not raw JSON.
	type canonical struct {
		Tool           instancepresence.ToolKind       `json:"tool"`
		HookSessionRef instancepresence.OpaqueIdentity `json:"hook_session_ref"`
		Lifecycle      instancecorrelation.Lifecycle   `json:"lifecycle"`
	}
	data, err := json.Marshal(canonical{
		Tool:           payload.Tool,
		HookSessionRef: payload.HookSessionRef,
		Lifecycle:      payload.Lifecycle,
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func peekProtocolVersion(data []byte) (ProtocolVersion, error) {
	if len(data) == 0 {
		return 0, protocolError(CodeMalformedRequest, errors.New("empty request"))
	}
	var envelope struct {
		ProtocolVersion ProtocolVersion `json:"protocol_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return 0, protocolError(CodeMalformedRequest, err)
	}
	return envelope.ProtocolVersion, nil
}

func ensureIngestNoBinding(response IngestResponse) IngestResponse {
	response.NoBindingPerformed = true
	response.ProtocolVersion = IngestProtocolVersion
	if response.ErrorCodes == nil {
		response.ErrorCodes = []ErrorCode{}
	}
	return response
}

func sortedErrorCodes(codes []ErrorCode) []ErrorCode {
	if len(codes) == 0 {
		return []ErrorCode{}
	}
	out := append([]ErrorCode{}, codes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
