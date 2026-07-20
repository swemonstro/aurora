package publish

import (
	"context"

	"github.com/swemonstro/aurora/internal/presence"
)

type Publisher interface {
	Publish(context.Context, presence.Snapshot) error
}
