package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/pkg/api"
)

// EventFilter selects which events Subscribe delivers.
// An empty Types list delivers every event. Filtering is also applied by the
// daemon when Types is set, and again here so a resumed stream cannot leak
// other types.
type EventFilter struct {
	Types []api.EventType
}

// Subscribe streams GET /v1/events until ctx is cancelled.
//
// The first connection is live-only. If the stream drops, the client reconnects
// with ?after= set to the last event ID it observed, so the daemon replays
// anything still in its ring. The returned channel is closed when ctx is done
// or the daemon rejects the stream (400 or 401).
//
// Start Subscribe, then take snapshots. Apply an event only when its request
// id or activity id is newer than the snapshot, so a step that arrived during
// the snapshot is not added twice.
func (c *Client) Subscribe(ctx context.Context, filter EventFilter) (<-chan api.Event, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	out := make(chan api.Event, 64)
	go c.subscribeLoop(ctx, filter, out)
	return out, nil
}

func (c *Client) subscribeLoop(ctx context.Context, filter EventFilter, out chan<- api.Event) {
	defer close(out)
	after := int64(-1)
	backoff := 50 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.streamOnce(ctx, after, filter, out, &after)
		if ctx.Err() != nil {
			return
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnauthorized) {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, after int64, filter EventFilter, out chan<- api.Event, last *int64) error {
	resp, err := c.do(ctx, http.MethodGet, eventsPath(after, filter), nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decode(resp, nil)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var id int64
	var data []byte
	flush := func() error {
		if len(data) == 0 {
			id = 0
			return nil
		}
		var ev api.Event
		if err := json.Unmarshal(data, &ev); err == nil {
			if ev.ID == 0 {
				ev.ID = id
			}
			if ev.ID > 0 {
				*last = ev.ID
			}
			if acceptEvent(filter, ev) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- ev:
				}
			}
		}
		data = nil
		id = 0
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "id:") {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64)
			if err == nil {
				id = n
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
	}
	if err := sc.Err(); err != nil {
		_ = flush()
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return io.EOF
}

func eventsPath(after int64, filter EventFilter) string {
	q := url.Values{}
	if after >= 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	for _, t := range filter.Types {
		if t != "" {
			q.Add("type", string(t))
		}
	}
	if len(q) == 0 {
		return "/v1/events"
	}
	return "/v1/events?" + q.Encode()
}

func acceptEvent(filter EventFilter, ev api.Event) bool {
	if len(filter.Types) == 0 {
		return true
	}
	for _, t := range filter.Types {
		if ev.Type == t {
			return true
		}
	}
	return false
}
