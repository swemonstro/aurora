package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/swemonstro/aurora/internal/presence"
)

type JSONPublisher struct {
	writer io.Writer
}

func NewJSONPublisher(writer io.Writer) (*JSONPublisher, error) {
	if writer == nil {
		return nil, fmt.Errorf("writer must not be nil")
	}

	return &JSONPublisher{writer: writer}, nil
}

func (p *JSONPublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	encoder := json.NewEncoder(p.writer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(snapshot); err != nil {
		return fmt.Errorf("encode presence snapshot: %w", err)
	}

	return nil
}
