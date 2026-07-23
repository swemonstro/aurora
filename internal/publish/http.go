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
	"github.com/swemonstro/aurora/internal/presencev2"
)

const maxErrorResponseBytes = 4 * 1024

type HTTPPublisher struct {
	endpoint             string
	presentationEndpoint string
	client               *http.Client
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

	baseURL := strings.TrimRight(relayURL, "/")

	return &HTTPPublisher{
		endpoint:             baseURL + "/presence",
		presentationEndpoint: baseURL + "/presence/presentation",
		client:               client,
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

	return p.do(request, "post presence snapshot to relay")
}

func (p *HTTPPublisher) PublishPresentation(
	ctx context.Context,
	presentation presencev2.Presentation,
) error {
	if err := presentation.Validate(); err != nil {
		return fmt.Errorf(
			"validate presence presentation: %w",
			err,
		)
	}

	body, err := json.Marshal(presentation)
	if err != nil {
		return fmt.Errorf(
			"encode presence presentation: %w",
			err,
		)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.presentationEndpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf(
			"create presentation request: %w",
			err,
		)
	}
	request.Header.Set("Content-Type", "application/json")

	return p.do(
		request,
		"post presence presentation to relay",
	)
}

func (p *HTTPPublisher) RemovePresentation(
	ctx context.Context,
) error {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		p.presentationEndpoint,
		nil,
	)
	if err != nil {
		return fmt.Errorf(
			"create presentation removal request: %w",
			err,
		)
	}

	return p.do(
		request,
		"delete presence presentation from relay",
	)
}

func (p *HTTPPublisher) Remove(ctx context.Context, source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("source must not be empty")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.endpoint, nil)
	if err != nil {
		return fmt.Errorf("create relay removal request: %w", err)
	}
	query := request.URL.Query()
	query.Set("source", source)
	request.URL.RawQuery = query.Encode()

	return p.do(request, "delete presence source from relay")
}

func (p *HTTPPublisher) do(request *http.Request, operation string) error {
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
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
