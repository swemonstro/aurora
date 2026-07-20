package presence

import (
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

const ProtocolVersion = 1

type Snapshot struct {
	Version   int          `json:"version"`
	Source    string       `json:"source"`
	State     status.State `json:"state"`
	Timestamp time.Time    `json:"timestamp"`
}
