package events

import (
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// SystemStart builds a Factory for the `system.start` lifecycle event — the
// request to start the VM or container has been received and boot is beginning.
func SystemStart() Factory {
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromSystemStartEvent(gen.SystemStartEvent{Id: int(id), Timestamp: ts})
		return ev
	}
}

// SystemShutdown builds a Factory for the `system.shutdown` lifecycle event —
// the sandbox expired its TTL without activity and is being torn down.
func SystemShutdown() Factory {
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromSystemShutdownEvent(gen.SystemShutdownEvent{Id: int(id), Timestamp: ts})
		return ev
	}
}

// SystemVmResumed builds a Factory for the `system.vm-resumed` lifecycle event —
// a microVM was resumed from a snapshot instead of cold booting.
func SystemVmResumed() Factory {
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromSystemVmResumedEvent(gen.SystemVmResumedEvent{Id: int(id), Timestamp: ts})
		return ev
	}
}

// SystemConfigChanged builds a Factory for the `system.config-changed` lifecycle
// event, recording the full config as of the change.
func SystemConfigChanged(cfg gen.SandboxConfig) Factory {
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromSystemConfigChangedEvent(gen.SystemConfigChangedEvent{
			Id:        int(id),
			Timestamp: ts,
			Config:    cfg,
		})
		return ev
	}
}
