package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/publish"
)

func main() {
	event := flag.String("event", "", "local event to normalize")
	source := flag.String("source", "test", "integration that produced the event")
	flag.Parse()

	if strings.TrimSpace(*event) == "" {
		fmt.Fprintln(os.Stderr, "usage: aurora-agent -event <event> [-source <source>]")
		os.Exit(2)
	}

	publisher, err := publish.NewJSONPublisher(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create publisher:", err)
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
