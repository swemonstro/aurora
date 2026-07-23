package localhooktransport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/sessionbinding"
)

// RuntimeLinkResult is the strict production link outcome for one peer capture.
type RuntimeLinkResult struct {
	RuntimeRef instancepresence.InstanceID
	Unique     bool
	Rule       string
	Reasons    []string
	TimedOut   bool
}

// RuntimeLinker resolves a unique verified runtime for a peer capture.
// Implementations must be fail-closed: Unique=true only for exactly one match.
type RuntimeLinker interface {
	Link(ctx context.Context, capture IdentityPeerCapture, tool instancepresence.ToolKind) RuntimeLinkResult
}

// HookMutator applies sequenced hook state to a registered runtime instance.
// HookRevision is assigned atomically by the registry (ApplyNextHookMutation),
// not by the ingress stream revision.
type HookMutator interface {
	Get(id instancepresence.InstanceID) (instancepresence.Instance, error)
	ApplyNextHookMutation(
		id instancepresence.InstanceID,
		producerEpoch instancepresence.ProducerEpoch,
		state instancepresence.EffectiveState,
		observedAt time.Time,
		idempotencyKey string,
	) (instancepresence.Instance, error)
}

// BindingEngine implements IngestBinder with transactional session binding:
// for new sessions, Reserve → mutate → Commit (or Rollback on failure).
type BindingEngine struct {
	sessions *sessionbinding.Registry
	linker   RuntimeLinker
	registry HookMutator
}

// NewBindingEngine constructs a fail-closed binding engine.
func NewBindingEngine(sessions *sessionbinding.Registry, linker RuntimeLinker, registry HookMutator) (*BindingEngine, error) {
	if sessions == nil {
		return nil, errors.New("session binding registry is required")
	}
	if linker == nil {
		return nil, errors.New("runtime linker is required")
	}
	if registry == nil {
		return nil, errors.New("hook mutator is required")
	}
	return &BindingEngine{sessions: sessions, linker: linker, registry: registry}, nil
}

// Bind implements IngestBinder.
func (engine *BindingEngine) Bind(ctx context.Context, accepted AcceptedIngress, capture IdentityPeerCapture) IngestResponse {
	if ctx == nil {
		ctx = context.Background()
	}
	if accepted.Lifecycle == instancecorrelation.LifecycleEnded {
		return engine.bindEnded(accepted)
	}
	if existing, ok := engine.sessions.Lookup(accepted.Tool, accepted.HookSessionRef); ok {
		return engine.bindExisting(ctx, accepted, capture, existing)
	}
	return engine.bindNew(ctx, accepted, capture)
}

// bindNew: unique link → runtime registered → reserve → mutate → commit.
// Failed Get/mutation leaves no published binding.
func (engine *BindingEngine) bindNew(ctx context.Context, accepted AcceptedIngress, capture IdentityPeerCapture) IngestResponse {
	if err := ctx.Err(); err != nil {
		return emptyIngestResponse(StatusError, "", CodeHandlerTimeout)
	}

	link := engine.linker.Link(ctx, capture, accepted.Tool)
	if err := ctx.Err(); err != nil || link.TimedOut {
		return emptyIngestResponse(StatusError, "", CodeHandlerTimeout)
	}
	if !link.Unique {
		if hasReasonString(link.Reasons, "ambiguous_runtime_link") {
			return emptyIngestResponse(StatusRejected, "", CodeAmbiguousRuntimeLink)
		}
		return emptyIngestResponse(StatusRejected, "", CodeNoUniqueRuntimeLink)
	}

	if _, err := engine.registry.Get(link.RuntimeRef); err != nil {
		if errors.Is(err, instanceregistry.ErrNotFound) {
			return emptyIngestResponse(StatusRejected, "", CodeRuntimeNotRegistered)
		}
		return emptyIngestResponse(StatusError, "", CodeRegistryMutationFailed)
	}

	if err := engine.sessions.Reserve(accepted.Tool, accepted.HookSessionRef, link.RuntimeRef); err != nil {
		if errors.Is(err, sessionbinding.ErrConflict) || errors.Is(err, sessionbinding.ErrAlreadyBound) {
			return emptyIngestResponse(StatusRejected, "", CodeSessionBindingConflict)
		}
		return emptyIngestResponse(StatusRejected, "", CodeSessionBindingConflict)
	}

	if _, err := engine.registry.ApplyNextHookMutation(
		link.RuntimeRef,
		accepted.ProducerEpoch,
		accepted.State,
		accepted.ObservedAt,
		hookIdempotencyKey(accepted),
	); err != nil {
		_ = engine.sessions.Rollback(accepted.Tool, accepted.HookSessionRef)
		if errors.Is(err, instanceregistry.ErrNotFound) {
			return emptyIngestResponse(StatusRejected, "", CodeRuntimeNotRegistered)
		}
		return emptyIngestResponse(StatusError, "", CodeRegistryMutationFailed)
	}
	if err := engine.sessions.Commit(accepted.Tool, accepted.HookSessionRef); err != nil {
		// Mutation succeeded; commit failure is internal — binding not published.
		_ = engine.sessions.Rollback(accepted.Tool, accepted.HookSessionRef)
		return emptyIngestResponse(StatusError, "", CodeInternalError)
	}

	return IngestResponse{
		ProtocolVersion:    IngestProtocolVersion,
		Status:             StatusOK,
		ErrorCodes:         []ErrorCode{},
		NoBindingPerformed: false,
	}
}

// bindExisting reuses the committed RuntimeRef. A new unique link is optional
// and only used to detect identity conflicts; timeout/zero/ambiguous links keep
// the established binding.
func (engine *BindingEngine) bindExisting(
	ctx context.Context,
	accepted AcceptedIngress,
	capture IdentityPeerCapture,
	runtimeRef instancepresence.InstanceID,
) IngestResponse {
	// Best-effort re-measure: never drop an established binding on soft failures.
	if ctx.Err() == nil {
		link := engine.linker.Link(ctx, capture, accepted.Tool)
		if link.Unique && link.RuntimeRef != "" && link.RuntimeRef != runtimeRef {
			return emptyIngestResponse(StatusRejected, "", CodeSessionBindingConflict)
		}
		// Timed out, zero matches, ambiguous, or same runtime → keep binding.
	}

	if _, err := engine.registry.ApplyNextHookMutation(
		runtimeRef,
		accepted.ProducerEpoch,
		accepted.State,
		accepted.ObservedAt,
		hookIdempotencyKey(accepted),
	); err != nil {
		if errors.Is(err, instanceregistry.ErrNotFound) {
			return emptyIngestResponse(StatusRejected, "", CodeRuntimeNotRegistered)
		}
		return emptyIngestResponse(StatusError, "", CodeRegistryMutationFailed)
	}
	return IngestResponse{
		ProtocolVersion:    IngestProtocolVersion,
		Status:             StatusOK,
		ErrorCodes:         []ErrorCode{},
		NoBindingPerformed: false,
	}
}

// bindEnded looks up the binding, applies idle/release mutation first, then
// unbinds only after success. If the runtime is already gone or ended, the
// session binding is removed idempotently without requiring a mutation.
// Mutation failures keep the binding.
func (engine *BindingEngine) bindEnded(accepted AcceptedIngress) IngestResponse {
	runtimeRef, ok := engine.sessions.Lookup(accepted.Tool, accepted.HookSessionRef)
	if !ok {
		// Idempotent: no session to end.
		return emptyIngestResponse(StatusOK, "")
	}

	instance, err := engine.registry.Get(runtimeRef)
	if err != nil {
		if errors.Is(err, instanceregistry.ErrNotFound) {
			// Runtime already removed; drop the orphan session binding.
			if _, unbindErr := engine.sessions.Unbind(accepted.Tool, accepted.HookSessionRef); unbindErr != nil &&
				!errors.Is(unbindErr, sessionbinding.ErrNotBound) {
				return emptyIngestResponse(StatusError, "", CodeSessionBindingConflict)
			}
			return IngestResponse{
				ProtocolVersion:    IngestProtocolVersion,
				Status:             StatusOK,
				ErrorCodes:         []ErrorCode{},
				NoBindingPerformed: false,
			}
		}
		return emptyIngestResponse(StatusError, "", CodeRegistryMutationFailed)
	}
	if instance.Status == instancepresence.RuntimeEnded {
		// Runtime already ended; clear session binding without hook mutation.
		if _, unbindErr := engine.sessions.Unbind(accepted.Tool, accepted.HookSessionRef); unbindErr != nil &&
			!errors.Is(unbindErr, sessionbinding.ErrNotBound) {
			return emptyIngestResponse(StatusError, "", CodeSessionBindingConflict)
		}
		return IngestResponse{
			ProtocolVersion:    IngestProtocolVersion,
			Status:             StatusOK,
			ErrorCodes:         []ErrorCode{},
			NoBindingPerformed: false,
		}
	}

	if _, mutErr := engine.registry.ApplyNextHookMutation(
		runtimeRef,
		accepted.ProducerEpoch,
		instancepresence.StateIdle,
		accepted.ObservedAt,
		hookIdempotencyKey(accepted),
	); mutErr != nil {
		// Keep binding so a later retry can finish end semantics.
		return emptyIngestResponse(StatusError, "", CodeRegistryMutationFailed)
	}
	if _, unbindErr := engine.sessions.Unbind(accepted.Tool, accepted.HookSessionRef); unbindErr != nil &&
		!errors.Is(unbindErr, sessionbinding.ErrNotBound) {
		return emptyIngestResponse(StatusError, "", CodeSessionBindingConflict)
	}
	return IngestResponse{
		ProtocolVersion:    IngestProtocolVersion,
		Status:             StatusOK,
		ErrorCodes:         []ErrorCode{},
		NoBindingPerformed: false,
	}
}

// hookIdempotencyKey includes ingress stream identity (tool, session, epoch,
// stream revision) but not the runtime HookRevision, which the registry owns.
func hookIdempotencyKey(accepted AcceptedIngress) string {
	return fmt.Sprintf("%s|%s|%s|%d", accepted.Tool, accepted.HookSessionRef, accepted.ProducerEpoch, accepted.Revision)
}

func hasReasonString(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
