package localhooktransport

import (
	"context"
	"errors"
	"fmt"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
)

// RuntimeObservationSource is deliberately OS-neutral. Platform collection and
// agent recognition are composed before the receiver reaches this boundary.
type RuntimeObservationSource interface {
	Observe(context.Context) ([]instancecorrelation.RuntimeObservation, error)
}

type CorrelationEngine interface {
	Correlate(instancecorrelation.CorrelationInput) (instancecorrelation.CorrelationResult, error)
}

type CorrelationService struct {
	runtimes        RuntimeObservationSource
	correlator      CorrelationEngine
	clock           Clock
	maximumRuntimes int
}

func NewCorrelationService(runtimes RuntimeObservationSource, correlator CorrelationEngine, clock Clock, maximumRuntimes int) (*CorrelationService, error) {
	if runtimes == nil || correlator == nil || clock == nil {
		return nil, errors.New("runtime source, correlator, and clock are required")
	}
	if maximumRuntimes < 1 || maximumRuntimes > 12 {
		return nil, errors.New("maximum runtimes must be between 1 and 12")
	}
	return &CorrelationService{runtimes: runtimes, correlator: correlator, clock: clock, maximumRuntimes: maximumRuntimes}, nil
}

func (service *CorrelationService) Correlate(ctx context.Context, hook instancecorrelation.HookObservation) (instancecorrelation.CorrelationResult, error) {
	runtimes, err := service.runtimes.Observe(ctx)
	if err != nil {
		return instancecorrelation.CorrelationResult{}, fmt.Errorf("observe runtimes: %w", err)
	}
	if len(runtimes) > service.maximumRuntimes {
		return instancecorrelation.CorrelationResult{}, ErrRuntimeLimit
	}
	return service.correlator.Correlate(instancecorrelation.CorrelationInput{
		EvaluatedAt: canonicalTime(service.clock.Now()), Runtimes: runtimes,
		Hooks: []instancecorrelation.HookObservation{hook},
	})
}

type Receiver struct {
	config  Config
	service *CorrelationService
	replay  *replayCache
}

func NewReceiver(config Config, service *CorrelationService) (*Receiver, error) {
	if err := config.Validate(false); err != nil {
		return nil, err
	}
	if service == nil {
		return nil, errors.New("correlation service must not be nil")
	}
	return &Receiver{config: config, service: service, replay: newReplayCache(config.ReplayCapacity, config.ReplayTTL, config.Clock)}, nil
}

func (receiver *Receiver) HandleJSON(ctx context.Context, data []byte) Response {
	request, err := DecodeRequestJSON(data)
	if err != nil {
		return emptyResponse(StatusRejected, "", errorCode(err))
	}
	return receiver.Handle(ctx, request)
}

func (receiver *Receiver) Handle(ctx context.Context, request Request) Response {
	request = canonicalRequest(request)
	if err := ValidateRequest(receiver.config, request); err != nil {
		requestID := ""
		if validateOpaque("request ID", request.RequestID, receiver.config.MaximumRequestIDLength, true) == nil {
			requestID = request.RequestID
		}
		return emptyResponse(StatusRejected, requestID, errorCode(err))
	}
	if cached, exists, err := receiver.replay.lookup(request); err != nil {
		return emptyResponse(StatusRejected, request.RequestID, errorCode(err))
	} else if exists {
		cached.Status = StatusDuplicate
		cached.ErrorCodes = appendUniqueErrorCode(cached.ErrorCodes, CodeDuplicateRequest)
		return cached
	}

	result, err := receiver.service.Correlate(ctx, request.Observation.domain())
	if err != nil {
		response := emptyResponse(StatusError, request.RequestID, CodeCorrelationFailed)
		if errors.Is(err, ErrRuntimeLimit) {
			response.ErrorCodes = appendUniqueErrorCode(response.ErrorCodes, CodeInsufficientEvidence)
		}
		return response
	}
	response := sanitizedResponse(request.RequestID, result)
	if err := receiver.replay.store(request, response); err != nil {
		return emptyResponse(StatusRejected, request.RequestID, errorCode(err))
	}
	return response
}

func sanitizedResponse(requestID string, result instancecorrelation.CorrelationResult) Response {
	response := emptyResponse(StatusOK, requestID)
	response.Summary = result.Summary
	for _, proposal := range result.Proposals {
		response.Proposals = append(response.Proposals, Proposal{
			HookRef: proposal.HookRef, RuntimeRef: proposal.RuntimeRef, Score: proposal.Score,
			Confidence: proposal.Confidence, Reasons: proposal.Reasons,
			WouldBind: proposal.WouldBindUnderCurrentThreshold, Review: proposal.RequiresReview,
		})
	}
	for _, ambiguous := range result.Ambiguous {
		response.Ambiguous = append(response.Ambiguous, Ambiguous{
			HookRefs: append([]string{}, ambiguous.HookRefs...), RuntimeRefs: append([]string{}, ambiguous.RuntimeRefs...),
			BestScore: ambiguous.BestScore, Confidence: ambiguous.Confidence,
			Reasons: append([]instancecorrelation.ReasonCode{}, ambiguous.Reasons...),
		})
	}
	for _, rejected := range result.Rejected {
		response.Rejected = append(response.Rejected, Rejected{
			HookRef: rejected.HookRef, RuntimeRef: rejected.RuntimeRef,
			Reasons: append([]instancecorrelation.ReasonCode{}, rejected.Reasons...),
		})
	}
	for _, unmatched := range result.UnmatchedHooks {
		response.UnmatchedHooks = append(response.UnmatchedHooks, Unmatched{Ref: unmatched.HookRef, Reasons: append([]instancecorrelation.ReasonCode{}, unmatched.Reasons...)})
	}
	for _, unmatched := range result.UnmatchedRuntimes {
		response.UnmatchedRuntimes = append(response.UnmatchedRuntimes, Unmatched{Ref: unmatched.RuntimeRef, Reasons: append([]instancecorrelation.ReasonCode{}, unmatched.Reasons...)})
	}
	if len(response.Ambiguous) > 0 {
		response.ErrorCodes = appendUniqueErrorCode(response.ErrorCodes, CodeAmbiguousResult)
	}
	if len(response.Proposals) == 0 && len(response.Ambiguous) == 0 {
		response.ErrorCodes = appendUniqueErrorCode(response.ErrorCodes, CodeInsufficientEvidence)
	}
	return response
}

func appendUniqueErrorCode(codes []ErrorCode, code ErrorCode) []ErrorCode {
	for _, existing := range codes {
		if existing == code {
			return codes
		}
	}
	return append(codes, code)
}
