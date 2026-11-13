package occlient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Event is one OpenCode server-sent event.
type Event struct {
	ID       string          `json:"id,omitempty"`
	Created  int64           `json:"created,omitempty"`
	Type     string          `json:"type"`
	Location Location        `json:"location,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// DecodeData unmarshals the event payload into dst.
func (e Event) DecodeData(dst any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, dst)
}

// StreamEvents subscribes to /api/event and sends events to out until the
// context ends or the stream fails. Callers reconnect with backoff: the
// stream is live-only and events during a disconnect are missed.
func (c *Client) StreamEvents(ctx context.Context, types []string, out chan<- Event) error {
	base, password := c.Endpoint()
	if base == "" {
		return fmt.Errorf("opencode endpoint is not configured")
	}

	u, err := url.Parse(base + "/api/event")
	if err != nil {
		return err
	}
	q := u.Query()
	for _, t := range types {
		if t != "" {
			q.Add("types", t)
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if password != "" {
		req.SetBasicAuth("opencode", password)
	}

	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &APIError{Status: resp.StatusCode, Body: string(data), Message: decodeErrorMessage(data)}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var dataLines []string
	emit := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]

		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return true // ignore malformed frames instead of dropping the stream
		}
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !emit() {
				return ctx.Err()
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / heartbeat
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			dataLines = append(dataLines, v)
		}
		// event:, id:, retry: are not needed: the JSON frame carries the type.
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if !emit() {
		return ctx.Err()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.EOF
}

// EventTypes is the set the daemon needs. It includes the streaming deltas
// used by /live; they are cheap to handle and only buffered while a chat has
// an active live stream.
var EventTypes = []string{
	"session.created",
	"session.renamed",
	"session.deleted",
	"session.idle",
	"session.status",
	"session.execution.started",
	"session.execution.succeeded",
	"session.execution.failed",
	"session.execution.interrupted",
	"session.text.started",
	"session.text.delta",
	"session.text.ended",
	"session.tool.input.started",
	"session.tool.success",
	"session.tool.failed",
	"session.shell.started",
	"form.created",
	"form.replied",
	"form.cancelled",
	"permission.asked",
	"permission.replied",
}

// ParseSSEFrame parses a single SSE data payload. It is exported for tests.
func ParseSSEFrame(payload []byte) (Event, error) {
	var ev Event
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&ev); err != nil {
		return Event{}, err
	}
	return ev, nil
}
