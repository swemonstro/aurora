package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/relay"
)

const shutdownTimeout = 5 * time.Second

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *address, log.Default()); err != nil {
		fmt.Fprintln(os.Stderr, "run Aurora relay:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, address string, logger *log.Logger) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", address, err)
	}

	return serve(ctx, listener, logger)
}

func serve(ctx context.Context, listener net.Listener, logger *log.Logger) error {
	server, err := newServer()
	if err != nil {
		_ = listener.Close()
		return err
	}

	logger.Printf("Aurora relay listening on %s", listener.Addr())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}

		err := <-serveErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	}
}

func newServer() (*http.Server, error) {
	handler, err := relay.NewHandler(&relay.Store{})
	if err != nil {
		return nil, fmt.Errorf("create relay handler: %w", err)
	}

	return &http.Server{
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}, nil
}
