//go:build linux

package localhooktransport

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
)

// IngestClient sends one bounded Package 6 ingest_hook_event request.
type IngestClient struct {
	config IngestClientConfig
}

func NewIngestClient(config IngestClientConfig) (*IngestClient, error) {
	config.ExpectedUID = uint32(os.Geteuid())
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &IngestClient{config: config}, nil
}

// Send performs exactly one synchronous bounded delivery attempt.
// Callers that must fail open should use TryDeliverIngress instead.
func (client *IngestClient) Send(ctx context.Context, ingress hookadapter.IngressObservation) (IngestResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	totalDeadline := started.Add(client.config.TotalBudget)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(totalDeadline) {
		totalDeadline = deadline
	}
	ctx, cancel := context.WithDeadline(ctx, totalDeadline)
	defer cancel()

	request, err := NewIngestRequest(ingress)
	if err != nil {
		return IngestResponse{}, err
	}
	if err := ValidateIngestRequest(client.config, request); err != nil {
		return IngestResponse{}, err
	}
	payload, err := EncodeIngestRequestJSON(request)
	if err != nil {
		return IngestResponse{}, err
	}
	if uint64(len(payload)) > uint64(client.config.MaximumRequestBytes) {
		return IngestResponse{}, ErrRequestTooLarge
	}

	connection, err := client.dial(ctx, totalDeadline)
	if err != nil {
		return IngestResponse{}, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()

	if err := client.writeRequest(connection, totalDeadline, payload); err != nil {
		return IngestResponse{}, err
	}
	responseData, err := client.readResponse(connection, totalDeadline)
	if err != nil {
		return IngestResponse{}, err
	}
	response, err := DecodeIngestResponseJSON(responseData)
	if err != nil {
		return IngestResponse{}, err
	}
	if err := ValidateIngestResponse(response, request.RequestID); err != nil {
		return IngestResponse{}, err
	}
	_ = clientLatencyBucket(time.Since(started), false)
	return response, nil
}

// TryDeliverIngress is the fail-open composition helper for hook commands.
// When the feature flag is off, socket config is missing or insecure, ingress
// is invalid, entropy is unavailable, or transport fails, delivery is skipped
// and no error is returned.
func TryDeliverIngress(ctx context.Context, getenv func(string) string, ingress hookadapter.IngressObservation) {
	_ = DeliverIngress(ctx, getenv, ingress)
}

// DeliverIngress performs best-effort Package 6 delivery and returns an error
// only for diagnostics. Hook commands must ignore the error for exit status.
func DeliverIngress(ctx context.Context, getenv func(string) string, ingress hookadapter.IngressObservation) error {
	if getenv == nil || !LocalHookEnabled(getenv(EnvLocalHookEnabled)) {
		return nil
	}
	// Absolute total budget starts before socket config is read.
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	totalDeadline := started.Add(DefaultIngestTotalBudget)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(totalDeadline) {
		totalDeadline = deadline
	}
	ctx, cancel := context.WithDeadline(ctx, totalDeadline)
	defer cancel()

	if err := ingress.Validate(); err != nil {
		return err
	}
	socketPath, err := ResolveLocalHookSocket(getenv)
	if err != nil {
		return err
	}
	if err := validateClientSocketPath(socketPath); err != nil {
		return err
	}
	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	remaining := time.Until(totalDeadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if remaining < config.TotalBudget {
		config.TotalBudget = remaining
	}
	client, err := NewIngestClient(config)
	if err != nil {
		return err
	}
	_, err = client.Send(ctx, ingress)
	return err
}

func (client *IngestClient) dial(ctx context.Context, totalDeadline time.Time) (*net.UnixConn, error) {
	if err := ensureDeadlineRemaining(totalDeadline); err != nil {
		return nil, err
	}
	preIdentity, preErr := statSocket(client.config.SocketPath)
	if preErr != nil && errors.Is(preErr, ErrInsecureSocketPath) {
		if _, statErr := os.Lstat(client.config.SocketPath); statErr == nil {
			return nil, ErrInsecureSocketPath
		}
	}

	dialCtx, cancel := context.WithDeadline(ctx, phaseDeadline(totalDeadline, client.config.ConnectDeadline))
	defer cancel()
	dialer := net.Dialer{}
	raw, err := dialer.DialContext(dialCtx, "unix", client.config.SocketPath)
	if err != nil {
		if dialCtx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(dialCtx.Err(), context.DeadlineExceeded)) {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	connection, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return nil, errors.New("unix connection required")
	}

	peer, err := peerIdentity(connection)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if peer.UID != client.config.ExpectedUID {
		_ = connection.Close()
		return nil, ErrUnauthorizedPeer
	}

	if preErr == nil {
		postIdentity, postErr := statSocket(client.config.SocketPath)
		if postErr != nil || postIdentity != preIdentity {
			_ = connection.Close()
			return nil, ErrInsecureSocketPath
		}
	}
	return connection, nil
}

func (client *IngestClient) writeRequest(connection *net.UnixConn, totalDeadline time.Time, payload []byte) error {
	if err := ensureDeadlineRemaining(totalDeadline); err != nil {
		return err
	}
	deadline := phaseDeadline(totalDeadline, client.config.WriteDeadline)
	if err := connection.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := writeFrame(connection, payload, client.config.MaximumRequestBytes); err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return ErrRequestTooLarge
		}
		return err
	}
	return nil
}

func (client *IngestClient) readResponse(connection *net.UnixConn, totalDeadline time.Time) ([]byte, error) {
	if err := ensureDeadlineRemaining(totalDeadline); err != nil {
		return nil, err
	}
	deadline := phaseDeadline(totalDeadline, client.config.ReadDeadline)
	if err := connection.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	data, err := readFrame(connection, client.config.MaximumResponseBytes)
	if errors.Is(err, ErrRequestTooLarge) {
		return nil, ErrResponseTooLarge
	}
	return data, err
}

func phaseDeadline(totalDeadline time.Time, phaseCap time.Duration) time.Time {
	now := time.Now()
	phaseEnd := now.Add(phaseCap)
	if phaseEnd.After(totalDeadline) {
		return totalDeadline
	}
	return phaseEnd
}

func ensureDeadlineRemaining(totalDeadline time.Time) error {
	if !time.Now().Before(totalDeadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func validateClientSocketPath(socketPath string) error {
	if err := validateSocketPathSyntax(socketPath); err != nil {
		return err
	}
	directory := filepath.Dir(socketPath)
	if _, err := os.Lstat(directory); err != nil {
		// Missing parent is treated as unavailable delivery target, not a hard config fault.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return validatePrivateDirectory(directory, uint32(os.Geteuid()))
}
