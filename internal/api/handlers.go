package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
)

type Handlers struct {
	broker *events.Broker
	store  *ConfigStore
}

func NewHandlers(broker *events.Broker, store *ConfigStore) *Handlers {
	return &Handlers{broker: broker, store: store}
}

func (h *Handlers) GetConfig(c *gin.Context) {
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

// ApplyConfig diffs the desired config against the current on-disk
// state, writes the new config, emits a ConfigApplyEvent carrying the
// delta, and returns the post-apply state. Policy enforcers (sbxfuse,
// sbxproxy) subscribe to the event stream and reconcile their in-memory
// rules from the delta — this handler does not call them directly.
func (h *Handlers) ApplyConfig(c *gin.Context) {
	var desired gen.SandboxConfig
	if err := c.ShouldBindJSON(&desired); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	changes, applyErr := h.store.Apply(desired)
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

// GetEvents implements the long-lived SSE stream at GET /v1/events.
// Resume semantics: prefer the SSE-standard `Last-Event-ID` header
// (browsers send it automatically on EventSource reconnect); fall back
// to the `lastEventId` query param.
func (h *Handlers) GetEvents(c *gin.Context, params gen.GetEventsParams) {
	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	after := int64(0)
	if params.LastEventId != nil {
		after = int64(*params.LastEventId)
	}
	if hdr := c.GetHeader("Last-Event-ID"); hdr != "" {
		if parsed, err := strconv.ParseInt(hdr, 10, 64); err == nil {
			after = parsed
		}
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := h.broker.Subscribe(after)
	defer cancel()

	notify := c.Request.Context().Done()
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEFrame(w, entry); err != nil {
				return
			}
			flusher.Flush()
		case <-notify:
			return
		}
	}
}

// writeSSEFrame emits a single SSE event:
//
//	id: <int>
//	data: <SandboxEvent JSON>
//	<blank line>
//
// `id:` mirrors the entry id so SSE-aware clients (browsers) resume
// automatically via `Last-Event-ID` on reconnect.
func writeSSEFrame(w io.Writer, entry events.Entry) error {
	body, err := entry.Event.MarshalJSON()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("id: " + strconv.FormatInt(entry.ID, 10) + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}
