package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/swemonstro/aurora/internal/presence"
)

const maxErrorResponseBytes = 4 * 1024

type HTTPPublisher struct {
	endpoint string
	client   *http.Client
}

func NewHTTPPublisher(relayURL string, client *http.Client) (*HTTPPublisher, error) {
	relayURL = strings.TrimSpace(relayURL)
	if relayURL == "" {
		return nil, fmt.Errorf("relay URL must not be empty")
	}
	if client == nil {
		return nil, fmt.Errorf("HTTP client must not be nil")
	}

	parsedURL, err := url.ParseRequestURI(relayURL)
	if err != nil {
		return nil, fmt.Errorf("invalid relay URL %q: %w", relayURL, err)
	}
	if (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return nil, fmt.Errorf("relay URL must be an absolute HTTP or HTTPS URL")
	}

	return &HTTPPublisher{
		endpoint: strings.TrimRight(relayURL, "/") + "/presence",
		client:   client,
	}, nil
}

func (p *HTTPPublisher) Publish(ctx context.Context, snapshot presence.Snapshot) error {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode presence snapshot: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create relay request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("post presence snapshot to relay: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorResponseBytes))
	if readErr != nil {
		return fmt.Errorf("relay returned %s (read response body: %v)", response.Status, readErr)
	}
	if message := strings.TrimSpace(string(responseBody)); message != "" {
		return fmt.Errorf("relay returned %s: %s", response.Status, message)
	}
	return fmt.Errorf("relay returned %s", response.Status)
}
