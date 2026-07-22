//go:build linux

package localhooktransport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

type LogEvent struct {
	Accepted       bool
	Status         ResponseStatus
	Protocol       ProtocolVersion
	DurationBucket string
	RuntimeCount   int
	Confidence     string
	ReasonCodes    []string
}

type Logger interface {
	Log(LogEvent)
}

type Server struct {
	config        Config
	receiver      *Receiver
	ingest        *IngestReceiver
	authenticator Authenticator
	logger        Logger
	listener      *ownedListener
	semaphore     chan struct{}
	done          chan struct{}
	closeFinished chan struct{}
	closeOnce     sync.Once
	closeErr      error
	wait          sync.WaitGroup
}

func NewServer(config Config, receiver *Receiver, authenticator Authenticator, logger Logger) (*Server, error) {
	if err := config.Validate(true); err != nil {
		return nil, err
	}
	if receiver == nil || authenticator == nil {
		return nil, errors.New("receiver and authenticator are required")
	}
	listener, err := listenUnixSecure(config.SocketPath)
	if err != nil {
		return nil, err
	}
	return &Server{
		config: config, receiver: receiver, authenticator: authenticator, logger: logger,
		listener: listener, semaphore: make(chan struct{}, config.MaximumConcurrent), done: make(chan struct{}),
		closeFinished: make(chan struct{}),
	}, nil
}

// EnableIngest attaches Package 6 observe-only ingress handling to this server.
// Legacy v1 correlate_observation handling is unchanged.
func (server *Server) EnableIngest(config IngestServerConfig) error {
	if server == nil {
		return errors.New("server must not be nil")
	}
	receiver, err := NewIngestReceiver(config)
	if err != nil {
		return err
	}
	server.ingest = receiver
	return nil
}

// IngestReceiver returns the attached Package 6 receiver, if any.
func (server *Server) IngestReceiver() *IngestReceiver {
	if server == nil {
		return nil
	}
	return server.ingest
}

func (server *Server) Serve(ctx context.Context) error {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	for {
		connection, err := server.listener.listener.AcceptUnix()
		if err != nil {
			select {
			case <-server.done:
				return server.Close()
			default:
				return err
			}
		}
		server.authorizeAndDispatch(connection)
	}
}

func (server *Server) authorizeAndDispatch(connection *net.UnixConn) {
	peer, err := peerIdentity(connection)
	if err != nil || server.authenticator.Authenticate(peer) != nil {
		server.writeImmediate(connection, emptyResponse(StatusRejected, "", CodeUnauthorizedPeer))
		_ = connection.Close()
		server.log(LogEvent{Accepted: false, Status: StatusRejected, Protocol: CurrentProtocolVersion, ReasonCodes: []string{string(CodeUnauthorizedPeer)}})
		return
	}
	select {
	case server.semaphore <- struct{}{}:
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			defer func() { <-server.semaphore }()
			server.handleAuthenticated(connection)
		}()
	default:
		server.writeImmediate(connection, emptyResponse(StatusRejected, "", CodeConcurrencyLimit))
		_ = connection.Close()
		return
	}
}

func (server *Server) handleAuthenticated(connection *net.UnixConn) {
	defer connection.Close()
	started := server.config.Clock.Now()
	_ = connection.SetReadDeadline(started.Add(server.config.ReadDeadline))
	data, err := readFrame(connection, server.config.MaximumRequestBytes)
	if err != nil {
		server.writeImmediate(connection, emptyResponse(StatusRejected, "", errorCode(err)))
		return
	}

	version, versionErr := peekProtocolVersion(data)
	if versionErr == nil && version == IngestProtocolVersion {
		server.handleIngest(connection, data, started)
		return
	}

	handleCtx, cancel := context.WithTimeout(context.Background(), server.config.MaximumHandlingTime)
	response := server.receiver.HandleJSON(handleCtx, data)
	cancel()
	_ = connection.SetWriteDeadline(server.config.Clock.Now().Add(server.config.WriteDeadline))
	encoded, err := EncodeResponseJSON(response, server.config.MaximumResponseBytes)
	if err != nil {
		response = emptyResponse(StatusError, response.RequestID, CodeResponseTooLarge)
		encoded, _ = EncodeResponseJSON(response, server.config.MaximumResponseBytes)
	}
	_ = writeFrame(connection, encoded, server.config.MaximumResponseBytes)
	server.logEventForResponse(response, server.config.Clock.Now().Sub(started))
}

func (server *Server) handleIngest(connection *net.UnixConn, data []byte, started time.Time) {
	if server.ingest == nil {
		response := emptyIngestResponse(StatusRejected, "", CodeUnsupportedOperation)
		server.writeIngestImmediate(connection, response)
		server.logIngestEvent(response, server.config.Clock.Now().Sub(started))
		return
	}
	handleCtx, cancel := context.WithTimeout(context.Background(), server.ingest.config.MaximumHandlingTime)
	response := server.ingest.HandleJSON(handleCtx, data)
	cancel()
	_ = connection.SetWriteDeadline(server.config.Clock.Now().Add(server.config.WriteDeadline))
	encoded, err := EncodeIngestResponseJSON(response, server.ingest.config.MaximumResponseBytes)
	if err != nil {
		response = emptyIngestResponse(StatusError, response.RequestID, CodeResponseTooLarge)
		encoded, _ = EncodeIngestResponseJSON(response, server.ingest.config.MaximumResponseBytes)
	}
	_ = writeFrame(connection, encoded, server.ingest.config.MaximumResponseBytes)
	server.logIngestEvent(response, server.config.Clock.Now().Sub(started))
}

func (server *Server) writeIngestImmediate(connection *net.UnixConn, response IngestResponse) {
	_ = connection.SetWriteDeadline(server.config.Clock.Now().Add(server.config.WriteDeadline))
	maximum := uint32(DefaultIngestMaximumResponseBytes)
	if server.ingest != nil {
		maximum = server.ingest.config.MaximumResponseBytes
	}
	data, err := EncodeIngestResponseJSON(response, maximum)
	if err == nil {
		_ = writeFrame(connection, data, maximum)
	}
}

func (server *Server) logIngestEvent(response IngestResponse, duration time.Duration) {
	event := LogEvent{
		Accepted:       response.Status == StatusOK || response.Status == StatusDuplicate,
		Status:         response.Status,
		Protocol:       response.ProtocolVersion,
		DurationBucket: durationBucket(duration),
	}
	for _, code := range response.ErrorCodes {
		event.ReasonCodes = append(event.ReasonCodes, string(code))
	}
	server.log(event)
}

func (server *Server) writeImmediate(connection *net.UnixConn, response Response) {
	_ = connection.SetWriteDeadline(server.config.Clock.Now().Add(server.config.WriteDeadline))
	data, err := EncodeResponseJSON(response, server.config.MaximumResponseBytes)
	if err == nil {
		_ = writeFrame(connection, data, server.config.MaximumResponseBytes)
	}
}

func (server *Server) logEventForResponse(response Response, duration time.Duration) {
	event := LogEvent{
		Accepted: response.Status == StatusOK || response.Status == StatusDuplicate,
		Status:   response.Status, Protocol: response.ProtocolVersion,
		DurationBucket: durationBucket(duration), RuntimeCount: response.Summary.Runtimes,
	}
	if len(response.Proposals) > 0 {
		event.Confidence = string(response.Proposals[0].Confidence)
	}
	for _, code := range response.ErrorCodes {
		event.ReasonCodes = append(event.ReasonCodes, string(code))
	}
	server.log(event)
}

func (server *Server) log(event LogEvent) {
	if server.logger != nil {
		server.logger.Log(event)
	}
}

func (server *Server) Close() error {
	server.closeOnce.Do(func() {
		close(server.done)
		if err := server.listener.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			server.closeErr = err
		}
		server.wait.Wait()
		if err := server.listener.cleanup(); err != nil && server.closeErr == nil {
			server.closeErr = err
		}
		close(server.closeFinished)
	})
	<-server.closeFinished
	return server.closeErr
}
