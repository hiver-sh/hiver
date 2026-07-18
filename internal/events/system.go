package events

import (
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// SystemFactory builds a Factory that publishes a SystemEvent for a
// sandbox lifecycle transition (start, config-changed, shutdown). cfg is
// recorded only for `system.config-changed`; pass nil for the others.
func SystemFactory(t gen.SystemEventType, cfg *gen.SandboxConfig) Factory {
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromSystemEvent(gen.SystemEvent{
			Id:        int(id),
			Timestamp: ts,
			Type:      t,
			Config:    cfg,
		})
		return ev
	}
}
