package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

// Replay sends a captured request back through this proxy and returns the
// new exchange plus whether it satisfied the optional expectations.
func (p *Proxy) Replay(requestID string, expectStatus int, expectBody string) (shared.VerifyResult, error) {
	if p.Sink == nil {
		return shared.VerifyResult{}, fmt.Errorf("request %q not found", requestID)
	}
	req := p.Sink.Resolve(shared.RequestID(requestID))
	if req == nil {
		return shared.VerifyResult{}, fmt.Errorf("request %q not found", requestID)
	}
	if req.Method == "CONNECT" || req.URL == "" {
		return shared.VerifyResult{}, fmt.Errorf("request %s cannot be replayed", req.ID)
	}
	token := fmt.Sprintf("replay-%d", time.Now().UnixNano())
	var body io.Reader
	if req.RequestBodyRef != "" {
		if b := p.Sink.GetBody(req.RequestBodyRef); len(b) > 0 {
			body = bytes.NewReader(b)
		}
	}
	out, err := http.NewRequest(req.Method, req.URL, body)
	if err != nil {
		return shared.VerifyResult{}, err
	}
	for k, v := range req.RequestHeaders {
		if isHopByHop(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		out.Header.Set(k, v)
	}
	out.Header.Set("X-Crabbro-Replay", token)

	proxyURL, err := url.Parse("http://" + p.Addr())
	if err != nil {
		return shared.VerifyResult{}, err
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	resp, err := client.Do(out)
	result := shared.VerifyResult{}
	if err != nil {
		result.Errors = []string{err.Error()}
		return result, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, shared.MaxBodySize))
	result.Status = resp.StatusCode
	result.ResponseBody = string(respBody)

	var found *shared.Request
	for _, cand := range p.Sink.List(0) {
		if cand.RequestHeaders["X-Crabbro-Replay"] == token || headerListHas(cand, token) {
			found = cand
		}
	}
	if found != nil {
		result.ReplayRequestID = found.ID
		result.RuleTrace = found.RuleTrace
		result.Status = found.Status
		for _, tr := range found.RuleTrace {
			if tr.Matched {
				result.MatchedRules = append(result.MatchedRules, string(tr.RuleID))
			}
		}
	}
	if expectStatus != 0 && result.Status != expectStatus {
		result.Errors = append(result.Errors, fmt.Sprintf("status %d, want %d", result.Status, expectStatus))
	}
	if expectBody != "" && !strings.Contains(result.ResponseBody, expectBody) {
		result.Errors = append(result.Errors, "response body does not contain the expected text")
	}
	if found != nil && len(result.MatchedRules) == 0 {
		result.Notes = append(result.Notes, "replay completed and no enabled rule matched")
	}
	result.OK = len(result.Errors) == 0
	p.Sink.NoteActivity("verify", "", string(result.ReplayRequestID), "", fmt.Sprintf("replay %s -> %d ok=%t", req.ID, result.Status, result.OK))
	return result, nil
}

func headerListHas(req *shared.Request, token string) bool {
	for _, h := range req.RequestHeaderList {
		if strings.EqualFold(h.Name, "X-Crabbro-Replay") {
			for _, v := range h.Values {
				if v == token {
					return true
				}
			}
		}
	}
	return false
}
