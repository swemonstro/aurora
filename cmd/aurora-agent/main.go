package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/status"
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

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(message); err != nil {
		fmt.Fprintln(os.Stderr, "encode status:", err)
		os.Exit(1)
	}
}
