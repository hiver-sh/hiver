package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
)

// GetInfo reports internal runtime facts about the sandbox that are determined
// at boot rather than configured — currently the isolation mechanism actually
// in use, which sandboxd selects automatically from the image (see
// isolation.Detect). This route is gated behind readiness, so the isolation
// backend is always wired by the time it is reachable.
func (h *Sandbox) GetInfo(c *gin.Context) {
	if h.iso == nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "isolation backend not initialized"})
		return
	}
	c.JSON(http.StatusOK, gen.SandboxInfo{
		Isolation: gen.SandboxInfoIsolation(h.iso.Kind()),
	})
}

func (h *Sandbox) GetConfig(c *gin.Context) {
	cfg, err := h.store.Get()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// freezeImmutable coerces fields of desired that can't change back to their
// current on-disk values, so applying a config that touches them is a silent
// no-op rather than a write that misrepresents the running sandbox.
//
//   - image defines the rootfs/VM and is fixed the moment the sandbox boots (a
//     prewarm sandbox receives it via env before its first apply). It is frozen
//     whenever already set, so an apply can only ever set it when unset, never
//     change it. (Isolation is no longer a config field — sandboxd derives it
//     from the image at boot; see isolation.Detect.)
//   - cpu, memory, entrypoint, cwd, tty and env are committed when the workload
//     launches, so they stay settable while the sandbox is still prewarm (not
//     started) and freeze afterward.
//
// fs, egress, ttl and snapshot are never frozen: those are reconciled at runtime.
func freezeImmutable(current, desired gen.SandboxConfig, started bool) gen.SandboxConfig {
	if current.Image != nil && *current.Image != "" {
		desired.Image = current.Image
	}
	if started {
		desired.Cpu = current.Cpu
		desired.Memory = current.Memory
		desired.Entrypoint = current.Entrypoint
		desired.Cwd = current.Cwd
		desired.Tty = current.Tty
		desired.Env = current.Env
	}
	return desired
}

// validateConfig returns an error if the config contains fields that sandboxd
// would reject at startup (e.g. relative mount paths, unknown backends).
func validateConfig(cfg gen.SandboxConfig) error {
	for i, fs := range cfg.Fs {
		base := fsBase(fs)
		if base.Mount == "" {
			return fmt.Errorf("fs[%d].mount is required", i)
		}
		if !strings.HasPrefix(base.Mount, "/") {
			return fmt.Errorf("fs[%d].mount: must be an absolute path, got %q", i, base.Mount)
		}
		if !base.Backend.Valid() {
			return fmt.Errorf("fs[%d].backend: unknown value %q", i, base.Backend)
		}
	}
	if cfg.Egress != nil {
		for i, rule := range *cfg.Egress {
			if !rule.Access.Valid() {
				return fmt.Errorf("egress[%d].access: must be %q or %q, got %q",
					i, gen.EgressRuleAccessAllow, gen.EgressRuleAccessDeny, rule.Access)
			}
		}
	}
	return nil
}

// ApplyConfig diffs the desired config against the current on-disk
// state, writes the new config, emits a ConfigApplyEvent carrying the
// delta, and returns the post-apply state. Policy enforcers (sbxfuse,
// sbxproxy) subscribe to the event stream and reconcile their in-memory
// rules from the delta — this handler does not call them directly.
func (h *Sandbox) ApplyConfig(c *gin.Context) {
	var desired gen.SandboxConfig
	if err := c.ShouldBindJSON(&desired); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	if err := validateConfig(desired); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	// Coerce fields that can't change back to their current values: image is
	// fixed at boot; cpu/memory/entrypoint/cwd/tty/env stay settable
	// only while the sandbox is still prewarm (not started). This is what lets a
	// --prewarm sandbox receive its full config from the first PUT /v1/config
	// while making a later change to a committed field a silent no-op.
	current, err := h.store.Get()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	desired = freezeImmutable(current, desired, h.Started())

	changes, applyErr := h.store.Apply(normalizeConfig(desired))
	if errors.Is(applyErr, ErrApplyInProgress) {
		c.JSON(http.StatusConflict, gen.Error{Error: applyErr.Error()})
		return
	}

	success := applyErr == nil
	postState := desired
	if !success {
		// Apply rolled back: report the pre-apply state as the post-apply
		// config so callers see the actual on-disk truth.
		if prev, err := h.store.Get(); err == nil {
			postState = prev
		}
	}

	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		evt := gen.ConfigApplyEvent{
			Id:        int(id),
			Timestamp: ts,
			Success:   success,
			Changes:   changes,
		}
		if applyErr != nil {
			msg := applyErr.Error()
			evt.ErrorMessage = &msg
		}
		_ = ev.FromConfigApplyEvent(evt)
		return ev
	})

	// On a committed change, also record a lifecycle event carrying the full
	// post-apply config (config.apply above only reports the delta).
	if success {
		h.broker.Publish(events.SystemConfigChanged(postState))
	}

	result := gen.ApplyResult{
		Applied: success,
		Config:  postState,
		Changes: changes,
	}
	if applyErr != nil {
		msg := applyErr.Error()
		result.Error = &msg
	}
	c.JSON(http.StatusOK, result)
}
