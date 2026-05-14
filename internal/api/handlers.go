package api

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
)

type Handlers struct {
	broker *events.Broker
}

func NewHandlers(broker *events.Broker) *Handlers {
	return &Handlers{broker: broker}
}

func (h *Handlers) GetConfig(c *gin.Context) {
	res := make(map[string]any)
	c.JSON(http.StatusOK, res)
}

func (h *Handlers) ApplyConfig(c *gin.Context) {

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
