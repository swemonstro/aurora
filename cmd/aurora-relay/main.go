package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/relay"
)

func main() {
	address := flag.String(
		"listen",
		"127.0.0.1:8080",
		"HTTP listen address",
	)
	flag.Parse()

	store := &relay.Store{}

	handler, err := relay.NewHandler(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create relay handler:", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              *address,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Aurora relay listening on %s", *address)

	if err := server.ListenAndServe(); err != nil &&
		err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
