package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// Offload invokes the gateway without passing through its external listener or
// token authentication. The request must already be isolated by the caller.
func (g *Gateway) Offload(req *http.Request) (*http.Response, error) {
	if req == nil || req.Context().Err() != nil {
		return nil, errors.New("gateway request canceled")
	}
	reader, writer := io.Pipe()
	ready := make(chan *http.Response, 1)
	stop := contextClose(req, reader)
	go func() {
		w := &offloadWriter{header: make(http.Header), pipe: writer, ready: ready}
		defer func() {
			if recover() != nil {
				_ = writer.CloseWithError(errors.New("gateway handler failed"))
				select {
				case ready <- nil:
				default:
				}
			} else {
				w.commit(http.StatusOK)
				_ = writer.Close()
			}
		}()
		g.serveIngress(w, req)
	}()
	select {
	case resp := <-ready:
		if resp == nil {
			stop()
			_ = reader.Close()
			return nil, errors.New("gateway handler failed")
		}
		resp.Body = &offloadBody{ReadCloser: reader, stop: stop}
		return resp, nil
	case <-req.Context().Done():
		stop()
		_ = reader.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
}

func contextClose(req *http.Request, reader *io.PipeReader) func() bool {
	return context.AfterFunc(req.Context(), func() { _ = reader.CloseWithError(req.Context().Err()) })
}

type offloadBody struct {
	io.ReadCloser
	stop func() bool
}

func (b *offloadBody) Close() error { b.stop(); return b.ReadCloser.Close() }

type offloadWriter struct {
	header    http.Header
	pipe      *io.PipeWriter
	ready     chan<- *http.Response
	committed bool
}

func (w *offloadWriter) Header() http.Header { return w.header }
func (w *offloadWriter) commit(status int) {
	if w.committed {
		return
	}
	w.committed = true
	w.ready <- &http.Response{StatusCode: status, Header: w.header.Clone()}
}
func (w *offloadWriter) WriteHeader(status int)      { w.commit(status) }
func (w *offloadWriter) Write(b []byte) (int, error) { w.commit(http.StatusOK); return w.pipe.Write(b) }
func (w *offloadWriter) Flush()                      { w.commit(http.StatusOK) }
