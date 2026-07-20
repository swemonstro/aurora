package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/status"
	"github.com/swemonstro/aurora/internal/transport"
)

func main() {
	event := flag.String("event", "", "local event to normalize")
	source := flag.String("source", "test", "integration that produced the event")
	flag.Parse()

	if strings.TrimSpace(*event) == "" {
		fmt.Fprintln(os.Stderr, "usage: aurora-agent -event <event> [-source <source>]")
		os.Exit(2)
	}

	state, err := status.Normalize(*event)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	message := status.Message{
		Version:   1,
		Source:    *source,
		State:     state,
		Timestamp: time.Now().UTC(),
	}

	sender, err := transport.NewJSONSender(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create transport:", err)
		os.Exit(1)
	}

	if err := sender.Send(context.Background(), message); err != nil {
		fmt.Fprintln(os.Stderr, "send status:", err)
		os.Exit(1)
	}
}
