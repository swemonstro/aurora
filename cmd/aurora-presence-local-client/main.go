//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/localhooktransport"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type requestSender interface {
	Send(context.Context, localhooktransport.Request) (localhooktransport.Response, error)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer, injected requestSender) error {
	flags := flag.NewFlagSet("aurora-presence-local-client", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "absolute path to the private Aurora Unix socket")
	inputPath := flags.String("input", "-", "sanitized request JSON file, or - for stdin")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Send one sanitized observation to the observe-only local correlator; no binding is performed.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-local-client -socket PATH [-input FILE]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *socketPath == "" && injected == nil {
		flags.Usage()
		return errors.New("socket is required")
	}
	reader := stdin
	var input *os.File
	if *inputPath != "-" {
		file, err := os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open explicit input: %w", err)
		}
		input = file
		defer input.Close()
		reader = input
	}
	limited := io.LimitReader(reader, 64*1024+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return errors.New("read sanitized request")
	}
	if len(data) > 64*1024 {
		return localhooktransport.ErrRequestTooLarge
	}
	request, err := localhooktransport.DecodeRequestJSON(data)
	if err != nil {
		return err
	}
	sender := injected
	if sender == nil {
		config := localhooktransport.DefaultConfig(systemClock{})
		config.SocketPath = *socketPath
		client, err := localhooktransport.NewClient(config)
		if err != nil {
			return err
		}
		sender = client
	}
	response, err := sender.Send(context.Background(), request)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(response)
}
