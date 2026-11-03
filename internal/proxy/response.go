package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

func splitStage(actions []shared.RuleAction) (request, response []shared.RuleAction) {
	for _, a := range actions {
		switch a.Type {
		case shared.ActionSetResponseHeader, shared.ActionRemoveResponseHeader,
			shared.ActionRewriteResponseBody, shared.ActionSetResponseStatus,
			shared.ActionDelay, shared.ActionThrottle:
			response = append(response, a)
		case shared.ActionReplaceFrame, shared.ActionDropFrame:
			// Applied when a WebSocket frame is relayed.
		default:
			request = append(request, a)
		}
	}
	return request, response
}

func previewResponse(resp *http.Response) ([]byte, bool) {
	if resp == nil || resp.Body == nil {
		return nil, false
	}
	preview, extra, _ := readPreview(resp.Body, shared.MaxBodySize)
	rest := io.Reader(resp.Body)
	if len(extra) > 0 {
		rest = io.MultiReader(bytes.NewReader(extra), resp.Body)
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(preview), rest))
	return preview, len(extra) > 0
}

// applyResponseActions mutates resp. On error the body is left readable and
// earlier header edits from this call are not committed.
func applyResponseActions(resp *http.Response, actions []shared.RuleAction) error {
	if resp == nil || len(actions) == 0 {
		return nil
	}
	hdr := resp.Header.Clone()
	if hdr == nil {
		hdr = make(http.Header)
	}
	status := resp.StatusCode
	var delay time.Duration
	var bps int
	replaceBody := false
	var newBody []byte

	needsBody := false
	for _, a := range actions {
		if a.Type == shared.ActionRewriteResponseBody {
			needsBody = true
		}
	}
	var full []byte
	if needsBody {
		buf, extra, err := readPreview(resp.Body, shared.MaxTransformBody)
		if err != nil && err != io.EOF {
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), bytes.NewReader(extra), resp.Body))
			return err
		}
		if len(extra) > 0 {
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), bytes.NewReader(extra), resp.Body))
			return errTransformTooLarge
		}
		rest, _ := io.ReadAll(resp.Body)
		full = append(append([]byte{}, buf...), rest...)
		resp.Body = io.NopCloser(bytes.NewReader(full))
	}

	for _, a := range actions {
		switch a.Type {
		case shared.ActionSetResponseHeader:
			name, _ := a.Params["name"].(string)
			value, _ := a.Params["value"].(string)
			hdr.Set(name, value)
		case shared.ActionRemoveResponseHeader:
			name, _ := a.Params["name"].(string)
			hdr.Del(name)
		case shared.ActionSetResponseStatus:
			status = intParam(a.Params["status"], status)
		case shared.ActionDelay:
			ms := intParam(a.Params["milliseconds"], 0)
			d := time.Duration(ms) * time.Millisecond
			if d > delay {
				delay = d
			}
		case shared.ActionThrottle:
			bps = intParam(a.Params["bytesPerSecond"], bps)
		case shared.ActionRewriteResponseBody:
			if body, ok := a.Params["body"].(string); ok {
				newBody = []byte(body)
				replaceBody = true
				continue
			}
			find, _ := a.Params["find"].(string)
			repl, _ := a.Params["replace"].(string)
			newBody = bytes.ReplaceAll(full, []byte(find), []byte(repl))
			replaceBody = true
		}
	}

	resp.Header = hdr
	resp.StatusCode = status
	resp.Status = http.StatusText(status)
	if replaceBody {
		if resp.Body != nil {
			resp.Body.Close()
		}
		resp.Body = io.NopCloser(bytes.NewReader(newBody))
		resp.ContentLength = int64(len(newBody))
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if bps > 0 && resp.Body != nil {
		resp.Body = &throttleReader{r: resp.Body, bps: bps, start: time.Now()}
	}
	return nil
}

var errTransformTooLarge = errString("response body exceeds transform limit")

type errString string

func (e errString) Error() string { return string(e) }

type throttleReader struct {
	r     io.Reader
	bps   int
	start time.Time
	sent  int
}

func (t *throttleReader) Read(p []byte) (int, error) {
	if t.bps <= 0 {
		return t.r.Read(p)
	}
	chunk := t.bps / 20
	if chunk < 1 {
		chunk = 1
	}
	if len(p) > chunk {
		p = p[:chunk]
	}
	elapsed := time.Since(t.start)
	expect := time.Duration(float64(t.sent) / float64(t.bps) * float64(time.Second))
	if expect > elapsed {
		time.Sleep(expect - elapsed)
	}
	n, err := t.r.Read(p)
	t.sent += n
	return n, err
}

func (t *throttleReader) Close() error {
	if c, ok := t.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func headerValue(h http.Header, name string) string {
	return strings.Join(h.Values(name), ", ")
}
