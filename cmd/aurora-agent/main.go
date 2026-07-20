package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/publish"
)

func main() {
	event := flag.String("event", "", "local event to normalize")
	source := flag.String("source", "test", "integration that produced the event")
	relayURL := flag.String("relay", "", "Aurora Relay base URL")
	flag.Parse()

	if strings.TrimSpace(*event) == "" || strings.TrimSpace(*relayURL) == "" {
		fmt.Fprintln(os.Stderr, "usage: aurora-agent -event <event> -relay <URL> [-source <source>]")
		os.Exit(2)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	publisher, err := publish.NewHTTPPublisher(*relayURL, client)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create HTTP publisher:", err)
		os.Exit(1)
	}

	instance, err := agent.New(*source, publisher, time.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create agent:", err)
		os.Exit(1)
	}

	if err := instance.Handle(context.Background(), *event); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
