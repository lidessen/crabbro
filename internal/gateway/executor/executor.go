// Package executor executes gateway requests against ranked upstream candidates
// with bounded request buffering, first-SSE-event peek failover, cooldowns, and
// scrubbed observation capture.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/ledger"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/scrub"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
	"github.com/lidessen/crabbro/internal/siwc"
	"github.com/lidessen/crabbro/internal/transform"
)

const (
	CapturePreview      = "preview"
	CaptureMetadataOnly = "metadata-only"
	CaptureOff          = "off"

	maxSSEPeekBytes = 64 * 1024
)

// BridgeHandler adapts cross-protocol Generative requests and Decision->Generative
// emulation requests across the canonical IRs.
type BridgeHandler interface {
	BuildUpstreamBody(cand codec.Target, ingress codec.IngressInfo, rawBody []byte) (upstreamBody []byte, state any, err error)
	TranslateResponse(cand codec.Target, ingress codec.IngressInfo, state any, status int, hdr http.Header, body []byte) (outStatus int, outHdr http.Header, outBody []byte, usage shared.TokenUsage, err error)
	TranslateStream(cand codec.Target, ingress codec.IngressInfo, state any, peeked []byte, src io.ReadCloser, dst io.Writer, flusher http.Flusher) (shared.TokenUsage, error)
	EncodeDecision(dreq *ir.DecisionRequest) ([]byte, error)
	DecodeDecisionResponse(cand codec.Target, state any, body []byte) (*ir.DecisionResponse, error)
}

// Executor executes routed HTTP and internal Decision calls.
type Executor struct {
	Router         *router.Router
	Secrets        *secret.Store
	Ledger         *ledger.Store
	Sink           capture.ExchangeSink
	Transports     *TransportPool
	Bridge         BridgeHandler
	CaptureMode    string
	MaxRequestBody int64
	SIWCClient     siwc.Client
	roundTrip      func(*http.Request) (*http.Response, error)

	// Signatures tracks in-memory thinking-signature provenance so the
	// same-protocol fast path can preserve signed history for the upstream
	// that minted it and strip it everywhere else.
	Signatures *codec.SignatureProvenance

	// AutoResetSignal is invoked when an upstream attempt is classified as
	// quota exhaustion or rate limiting. Implementations must not block: the
	// gateway uses it to raise the opt-in reset-coupon evaluation off the
	// request path.
	AutoResetSignal func(planID string)
}

// New constructs an Executor with sane defaults.
func New(rtr *router.Router, secrets *secret.Store, ldg *ledger.Store, sink capture.ExchangeSink) *Executor {
	return &Executor{
		Router:         rtr,
		Secrets:        secrets,
		Ledger:         ldg,
		Sink:           sink,
		Transports:     NewTransportPool(),
		CaptureMode:    CapturePreview,
		MaxRequestBody: shared.DefaultMaxGatewayRequestBody,
		Signatures:     codec.NewSignatureProvenance(),
	}
}

// ServeHTTP executes one client HTTP gateway call (with pre-commit failover and SSE peek).
func (e *Executor) ServeHTTP(w http.ResponseWriter, r *http.Request, snap *config.Snapshot, ingress codec.IngressInfo, rawBody []byte) {
	started := time.Now()
	sig := router.ExtractSignals(ingress.Profile, ingress.Protocol, ingress.Family, ingress.RequestedModel, ingress.SessionHint, rawBody)

	// Start capture record early (without secrets) if capture is enabled.
	rec := e.startRecord(r, snap, ingress, sig, rawBody, started)

	sel, err := e.Router.Select(r.Context(), snap, sig)
	if err != nil || sel == nil || len(sel.Candidates) == 0 {
		status := http.StatusBadGateway
		errMsg := "no matching gateway route"
		if err != nil {
			errMsg = err.Error()
		}
		var dec *shared.RouteDecision
		var routeID string
		if sel != nil {
			dec = sel.Decision
			if sel.Route != nil {
				routeID = sel.Route.ID
			}
		}
		if rec != nil {
			rec.Gateway.RouteID = routeID
			if dec != nil {
				clean := *dec
				clean.SessionKey = secret.ScrubText(clean.SessionKey, siwcValues(snap, e.Secrets)...)
				rec.Gateway.RouteDecision = &clean
			}
			rec.Gateway.TurnPhase = sig.TurnPhase
			rec.Gateway.SessionKey = secret.ScrubText(sig.SessionKey, siwcValues(snap, e.Secrets)...)
		}
		e.writeFailure(w, rec, ingress.Protocol, status, "gateway_routing_error", errMsg, started)
		return
	}

	values := siwcValues(snap, e.Secrets)
	sel.Signals.SessionKey = secret.ScrubText(sel.Signals.SessionKey, values...)
	if sel.Decision != nil {
		clean := *sel.Decision
		clean.SessionKey = secret.ScrubText(clean.SessionKey, values...)
		sel.Decision = &clean
	}
	if rec != nil {
		rec.Gateway.RouteID = sel.Route.ID
		rec.Gateway.TurnPhase = sig.TurnPhase
		rec.Gateway.SessionKey = secret.ScrubText(sig.SessionKey, siwcValues(snap, e.Secrets)...)
		rec.Gateway.CacheGravity = sel.CacheGravityMax
		rec.Gateway.RouteDecision = sel.Decision
	}

	maxAttempts := sel.Route.Policy.EffectiveMaxAttempts()
	sel.Candidates = expandCredentialCandidates(sel.Candidates)
	if maxAttempts > len(sel.Candidates) {
		maxAttempts = len(sel.Candidates)
	}

	var attempts []shared.GatewayAttempt
	accountedAttempts := 0
	minContextNeeded := 0
	var lastStatus int
	var lastHeaders http.Header
	var lastErrBody []byte
	var lastErrMsg string

	for i := 0; i < len(sel.Candidates) && len(attempts) < maxAttempts; i++ {
		cand := sel.Candidates[i]
		if e.candidateCooling(cand) {
			continue
		}
		if minContextNeeded > 0 && cand.ModelSpec != nil && cand.ModelSpec.ContextWindow > 0 && cand.ModelSpec.ContextWindow <= minContextNeeded {
			continue
		}

		res := e.tryCandidate(r, w, rec, cand, sel, ingress, rawBody, &attempts, &accountedAttempts, started)
		if res.committed {
			return
		}
		if res.status > 0 {
			lastStatus = res.status
			lastHeaders = res.headers
			lastErrBody = res.errBody
		}
		if res.errMsg != "" {
			lastErrMsg = res.errMsg
		}
		if !res.retryable {
			break
		}
		if res.contextMin > minContextNeeded {
			minContextNeeded = res.contextMin
		}
	}

	if rec != nil {
		rec.Gateway.Attempts = attempts
	}
	if lastStatus > 0 && len(lastErrBody) > 0 {
		e.writeUpstreamError(w, rec, attempts, lastStatus, lastHeaders, lastErrBody, started)
		return
	}
	if lastStatus == http.StatusUnauthorized && len(attempts) > 0 && attempts[len(attempts)-1].ErrorKind == ErrKindAuthError {
		e.writeFailure(w, rec, ingress.Protocol, http.StatusUnauthorized, ErrKindAuthError, lastErrMsg, started)
		return
	}
	if lastErrMsg == "" {
		lastErrMsg = "all gateway candidates failed"
	}
	e.writeFailure(w, rec, ingress.Protocol, http.StatusBadGateway, "upstream_error", lastErrMsg, started)
}

type attemptOutcome struct {
	committed  bool
	retryable  bool
	contextMin int
	status     int
	headers    http.Header
	errBody    []byte
	errMsg     string
}

func (e *Executor) tryCandidate(
	r *http.Request,
	w http.ResponseWriter,
	rec *shared.Request,
	cand router.Candidate,
	sel *router.Selection,
	ingress codec.IngressInfo,
	rawBody []byte,
	attempts *[]shared.GatewayAttempt,
	accountedAttempts *int,
	exStarted time.Time,
) attemptOutcome {
	attStart := time.Now()
	auth := e.authenticator(cand)
	err := auth.Load(r.Context())
	scrubVals := auth.ScrubValues()
	if err != nil {
		status := http.StatusBadGateway
		msg := "credential unavailable"
		retryable := true
		if !auth.DecisionAllowed() {
			status = http.StatusUnauthorized
			msg = secret.ScrubText(err.Error(), scrubVals...)
			retryable = false
		}
		*attempts = append(*attempts, shared.GatewayAttempt{Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Status: status, ErrorKind: ErrKindAuthError, ErrorMessage: msg})
		return attemptOutcome{status: status, retryable: retryable, errMsg: msg}
	}
	needBridge := cand.EndpointProto != ingress.Protocol
	if needBridge && e.Bridge == nil {
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:        cand.Target.Model,
			Protocol:     cand.EndpointProto,
			Status:       http.StatusNotImplemented,
			ErrorKind:    "bridge_unavailable",
			ErrorMessage: fmt.Sprintf("cross-protocol bridge %s -> %s not configured", ingress.Protocol, cand.EndpointProto),
		})
		return attemptOutcome{retryable: true, errMsg: "cross-protocol bridge unavailable"}
	}

	var upBody []byte
	var bridgeState any
	if !needBridge {
		upBody, err = codec.PatchModelAndExtraBody(rawBody, cand.EndpointProto, cand.Target.Model, cand.ModelSpec.ExtraBody)
		if err == nil {
			// Same-protocol failover can cross providers/model families. A
			// native body otherwise carries vendor signatures verbatim into
			// the next upstream, which rejects them as invalid.
			upBody, err = codec.NormalizeSameProtocolBody(upBody, cand.EndpointProto, codec.CandidateSignatureOrigin(cand), e.Signatures)
		}
	} else {
		upBody, bridgeState, err = e.Bridge.BuildUpstreamBody(cand, ingress, rawBody)
	}
	if err != nil {
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:        cand.Target.Model,
			Protocol:     cand.EndpointProto,
			Status:       http.StatusBadRequest,
			ErrorKind:    ErrKindClientBadRequest,
			ErrorMessage: secret.ScrubText(err.Error(), scrubVals...),
		})
		return attemptOutcome{retryable: false, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}

	upURL, err := codec.BuildUpstreamURL(cand.Endpoint.BaseURL, cand.EndpointProto, ingress.Action, cand.Target.Model, ingress.CleanQuery, ingress.Stream)
	if err != nil {
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:        cand.Target.Model,
			Protocol:     cand.EndpointProto,
			ErrorKind:    ErrKindUpstreamUnavailable,
			ErrorMessage: secret.ScrubText(err.Error(), scrubVals...),
		})
		return attemptOutcome{retryable: true, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}

	tr, err := e.Transports.ForProxy(cand.Plan.ProxyURL)
	if err != nil {
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:        cand.Target.Model,
			Protocol:     cand.EndpointProto,
			ErrorKind:    ErrKindUpstreamUnavailable,
			ErrorMessage: secret.ScrubText(err.Error(), scrubVals...),
		})
		return attemptOutcome{retryable: true, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}

	reqCtx, cancelReq := context.WithCancel(r.Context())
	defer cancelReq()
	ttftTimer := time.AfterFunc(cand.Plan.Timeout.TTFTDuration(), cancelReq)

	upReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, upURL, bytes.NewReader(upBody))
	if err != nil {
		ttftTimer.Stop()
		return attemptOutcome{retryable: true, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}
	upReq.Header = r.Header.Clone()
	stripProxyHopHeaders(upReq.Header)
	upReq.Header.Del("Accept-Encoding")
	upReq.Header.Set("Content-Type", "application/json")
	codec.InjectUpstreamAuth(upReq.Header, cand.Endpoint, cand.EndpointProto, "")
	if upReq.Header.Get("User-Agent") == "" {
		upReq.Header.Set("User-Agent", "crabbro-gateway/1.0")
	}
	upBody, err = transform.Request(upReq, upBody, cand.Endpoint.AuthHeader, cand.Endpoint.RequestTransform, cand.ModelSpec.RequestTransform)
	if err != nil {
		ttftTimer.Stop()
		*attempts = append(*attempts, shared.GatewayAttempt{Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Model: cand.Target.Model, Protocol: cand.EndpointProto, Status: http.StatusBadRequest, ErrorKind: ErrKindClientBadRequest, ErrorMessage: secret.ScrubText(err.Error(), scrubVals...)})
		return attemptOutcome{retryable: false, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}
	upBody, err = auth.Prepare(upReq, upBody)
	if err != nil {
		ttftTimer.Stop()
		return attemptOutcome{status: http.StatusUnauthorized, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}

	if e.Router != nil && e.Router.Health != nil {
		e.Router.Health.AcquireInFlight(cand.Plan.ID)
		defer e.Router.Health.ReleaseInFlight(cand.Plan.ID)
	}

	doRoundTrip := tr.RoundTrip
	if e.roundTrip != nil {
		doRoundTrip = e.roundTrip
	}
	resp, err := doRoundTrip(upReq)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		retry, authErr := auth.HandleUnauthorized(r.Context(), upReq, upBody)
		scrubVals = auth.ScrubValues()
		if authErr != nil {
			resp.Body.Close()
			ttftTimer.Stop()
			*attempts = append(*attempts, shared.GatewayAttempt{Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Status: http.StatusUnauthorized, ErrorKind: ErrKindAuthError, ErrorMessage: "SIWC refresh failed"})
			return attemptOutcome{status: http.StatusUnauthorized, errMsg: "SIWC refresh failed"}
		}
		if retry {
			resp.Body.Close()
			resp, err = doRoundTrip(upReq)
		}
	}

	ttftTimer.Stop()
	ttftMs := time.Since(attStart).Milliseconds()
	if err != nil {
		fc := ClassifyHTTPFailure(0, []byte(secret.ScrubText(err.Error(), scrubVals...)), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
		e.applyFailure(cand, fc, ttftMs, sel.Route.ID, sel.Signals.SessionKey, 0, accountedAttempts)
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:           cand.Target.Model,
			Protocol:        cand.EndpointProto,
			Status:          http.StatusBadGateway,
			ErrorKind:       fc.Kind,
			ErrorMessage:    secret.ScrubText(err.Error(), scrubVals...),
			LatencyMs:       ttftMs,
			CooldownApplied: fc.CooldownLabel,
		})
		return attemptOutcome{retryable: r.Context().Err() == nil, errMsg: secret.ScrubText(err.Error(), scrubVals...)}
	}

	if e.Ledger != nil && e.Ledger.Calibrator != nil {
		e.Ledger.Calibrator.UpdateHeaders(cand.Plan.ID, resp.Header, time.Now().UTC())
	}

	// Non-2xx response -> read bounded body, classify, cooldown, failover if retryable.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, readErr := transform.ReadResponse(resp.Body)
		resp.Body.Close()
		if readErr == nil && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header, errBody, readErr = transform.Response(resp.Header, errBody, cand.Endpoint.AuthHeader, cand.Endpoint.ResponseTransform, cand.ModelSpec.ResponseTransform)
			resp.Header = scrub.ScrubHeaders(resp.Header, scrubVals, cand.Endpoint.AuthHeader)
			errBody = []byte(scrub.ScrubText(string(errBody), scrubVals...))
		}
		resp.Header = scrub.ScrubHeaders(resp.Header, scrubVals, cand.Endpoint.AuthHeader)
		errBody = []byte(scrub.ScrubText(string(errBody), scrubVals...))
		if readErr != nil {
			latMs := time.Since(attStart).Milliseconds()
			fc := ClassifyHTTPFailure(http.StatusBadGateway, nil, nil, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, http.StatusBadGateway, accountedAttempts)
			*attempts = append(*attempts, shared.GatewayAttempt{
				Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
				Model:           cand.Target.Model,
				Protocol:        cand.EndpointProto,
				Status:          resp.StatusCode,
				ErrorKind:       fc.Kind,
				ErrorMessage:    "upstream response normalization failed",
				LatencyMs:       latMs,
				CooldownApplied: fc.CooldownLabel,
			})
			return attemptOutcome{retryable: true, errMsg: "upstream response normalization failed"}
		}
		latMs := time.Since(attStart).Milliseconds()
		fc := ClassifyHTTPFailure(resp.StatusCode, errBody, resp.Header, cand.Plan.ID, e.health(), cand.CredentialRef)
		e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, resp.StatusCode, accountedAttempts)

		ctxMin := 0
		if fc.ContextOnly && cand.ModelSpec != nil {
			ctxMin = cand.ModelSpec.ContextWindow
		}
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:           cand.Target.Model,
			Protocol:        cand.EndpointProto,
			Status:          resp.StatusCode,
			ErrorKind:       fc.Kind,
			ErrorMessage:    secret.ScrubText(fc.Summary, scrubVals...),
			LatencyMs:       latMs,
			CooldownApplied: fc.CooldownLabel,
		})
		return attemptOutcome{
			retryable:  fc.Retryable,
			contextMin: ctxMin,
			status:     resp.StatusCode,
			headers:    resp.Header.Clone(),
			errBody:    errBody,
			errMsg:     fc.Summary,
		}
	}

	if err := auth.ProcessResponse(resp, ingress.Stream); err != nil {
		return attemptOutcome{retryable: true, errMsg: err.Error()}
	}

	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if isSSE {
		peeked, sseRest, sseErrCode, sseErrMsg, hasSSEErr, emptyEOF := peekFirstSSE(resp.Body, cand.EndpointProto)
		if hasSSEErr || emptyEOF {
			resp.Body.Close()
			latMs := time.Since(attStart).Milliseconds()
			if emptyEOF {
				sseErrCode, sseErrMsg = "empty_stream", "upstream SSE stream closed with no events"
			}
			sseErrCode = secret.ScrubText(sseErrCode, scrubVals...)
			sseErrMsg = secret.ScrubText(sseErrMsg, scrubVals...)
			fc := ClassifySSEFailure(sseErrCode, sseErrMsg, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, http.StatusTooManyRequests, accountedAttempts)
			ctxMin := 0
			if fc.ContextOnly && cand.ModelSpec != nil {
				ctxMin = cand.ModelSpec.ContextWindow
			}
			*attempts = append(*attempts, shared.GatewayAttempt{
				Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
				Model:           cand.Target.Model,
				Protocol:        cand.EndpointProto,
				Status:          200,
				ErrorKind:       fc.Kind,
				ErrorMessage:    secret.ScrubText(sseErrMsg, scrubVals...),
				LatencyMs:       latMs,
				CooldownApplied: fc.CooldownLabel,
			})
			return attemptOutcome{retryable: fc.Retryable, contextMin: ctxMin, errMsg: secret.ScrubText(sseErrMsg, scrubVals...)}
		}

		// Commit streaming response downstream!
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:    cand.Target.Model,
			Protocol: cand.EndpointProto,
			Status:   resp.StatusCode,
		})

		e.commitStream(w, r, rec, cand, sel, ingress, *attempts, resp, peeked, sseRest, bridgeState, scrubVals, accountedAttempts, exStarted, attStart)
		return attemptOutcome{committed: true}
	}

	// Non-streaming 2xx response.
	defer resp.Body.Close()
	respBytes, readErr := transform.ReadResponse(resp.Body)
	if readErr == nil {
		resp.Header, respBytes, readErr = transform.Response(resp.Header, respBytes, cand.Endpoint.AuthHeader, cand.Endpoint.ResponseTransform, cand.ModelSpec.ResponseTransform)
		resp.Header = scrub.ScrubHeaders(resp.Header, scrubVals, cand.Endpoint.AuthHeader)
		respBytes = []byte(scrub.ScrubText(string(respBytes), scrubVals...))
	}
	latMs := time.Since(attStart).Milliseconds()
	if readErr != nil {
		fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(readErr.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
		e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, http.StatusBadGateway, accountedAttempts)
		*attempts = append(*attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
			Model:           cand.Target.Model,
			Protocol:        cand.EndpointProto,
			Status:          resp.StatusCode,
			ErrorKind:       fc.Kind,
			ErrorMessage:    secret.ScrubText(readErr.Error(), scrubVals...),
			LatencyMs:       latMs,
			CooldownApplied: fc.CooldownLabel,
		})
		return attemptOutcome{retryable: true, errMsg: secret.ScrubText(readErr.Error(), scrubVals...)}
	}

	outStatus := resp.StatusCode
	outHdr := resp.Header.Clone()
	outBody := respBytes
	var usage shared.TokenUsage
	if !needBridge {
		// Remember which exact upstream minted each thinking signature so the
		// next same-protocol turn can preserve it only for that target.
		e.Signatures.RecordResponse(cand.EndpointProto, codec.CandidateSignatureOrigin(cand), respBytes)
		if cand.EndpointProto == config.ProtocolOpenAIDecisions {
			dreq, decErr := codec.DecodeOpenAIDecisionsRequest(rawBody)
			if decErr == nil {
				_, decErr = codec.DecodeOpenAIDecisionsResponse(respBytes, dreq)
			}
			if decErr != nil {
				fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(decErr.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
				e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, http.StatusBadGateway, accountedAttempts)
				*attempts = append(*attempts, shared.GatewayAttempt{Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Model: cand.Target.Model, Protocol: cand.EndpointProto, Status: http.StatusBadGateway, ErrorKind: fc.Kind, ErrorMessage: decErr.Error(), LatencyMs: latMs})
				return attemptOutcome{retryable: true, errMsg: decErr.Error()}
			}
		}
		usage = codec.ExtractUsageJSON(cand.EndpointProto, respBytes)
	} else {
		var trErr error
		outStatus, outHdr, outBody, usage, trErr = e.Bridge.TranslateResponse(cand, ingress, bridgeState, resp.StatusCode, resp.Header, respBytes)
		if trErr != nil {
			fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(trErr.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, latMs, sel.Route.ID, sel.Signals.SessionKey, http.StatusBadGateway, accountedAttempts)
			*attempts = append(*attempts, shared.GatewayAttempt{
				Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
				Model:        cand.Target.Model,
				Protocol:     cand.EndpointProto,
				Status:       http.StatusBadGateway,
				ErrorKind:    fc.Kind,
				ErrorMessage: secret.ScrubText(trErr.Error(), scrubVals...),
				LatencyMs:    latMs,
			})
			return attemptOutcome{retryable: true, errMsg: secret.ScrubText(trErr.Error(), scrubVals...)}
		}
	}

	*attempts = append(*attempts, shared.GatewayAttempt{
		Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef,
		Model:     cand.Target.Model,
		Protocol:  cand.EndpointProto,
		Status:    outStatus,
		TTFTMs:    ttftMs,
		LatencyMs: latMs,
	})

	copyResponseHeaders(w.Header(), outHdr)
	if needBridge && outHdr.Get("X-Crabbro-Decision-Images") == "preserved" {
		w.Header().Set("X-Crabbro-Decision-Images", "preserved")
	}
	setTraceHeaders(w.Header(), sel.Route.ID, cand, len(*attempts), needBridge)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", strconv.Itoa(len(outBody)))
	w.WriteHeader(outStatus)
	_, _ = w.Write(outBody)

	e.recordSuccess(sel, cand, usage, ttftMs, time.Since(exStarted).Milliseconds(), 0, accountedAttempts)
	e.finalizeRecord(rec, cand, *attempts, usage, outStatus, outHdr, outBody, needBridge, scrubVals, exStarted, "")
	return attemptOutcome{committed: true}
}

func (e *Executor) commitStream(
	w http.ResponseWriter,
	r *http.Request,
	rec *shared.Request,
	cand router.Candidate,
	sel *router.Selection,
	ingress codec.IngressInfo,
	attempts []shared.GatewayAttempt,
	resp *http.Response,
	peeked []byte,
	sseRest io.Reader,
	bridgeState any,
	scrubVals []string,
	accountedAttempts *int,
	exStarted time.Time,
	attStart time.Time,
) {
	defer resp.Body.Close()
	needBridge := cand.EndpointProto != ingress.Protocol

	if !needBridge {
		copyResponseHeaders(w.Header(), resp.Header)
		w.Header().Del("Content-Length")
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}
	setTraceHeaders(w.Header(), sel.Route.ID, cand, len(attempts), needBridge)
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var usage shared.TokenUsage
	var preview bytes.Buffer
	var streamErr error
	timed := &streamTimingWriter{w: w, tap: codec.NewStreamDeltaTap(ingress.Protocol)}

	if !needBridge {
		tap := codec.NewUsageTap(cand.EndpointProto)
		var sigTap *codec.SignatureStreamTap
		if e.Signatures != nil {
			sigTap = codec.NewSignatureStreamTap(cand.EndpointProto, codec.CandidateSignatureOrigin(cand), e.Signatures)
		}
		dst := &streamSinkWriter{w: timed, f: flusher, tap: tap, preview: &preview, sigTap: sigTap}
		src := io.MultiReader(bytes.NewReader(peeked), sseRest)
		_, streamErr = io.Copy(dst, src)
		sigTap.Close()
		usage = tap.Usage()
	} else {
		capW := &limitedPreviewWriter{w: timed, preview: &preview}
		srcRC := struct {
			io.Reader
			io.Closer
		}{Reader: sseRest, Closer: resp.Body}
		usage, streamErr = e.Bridge.TranslateStream(cand, ingress, bridgeState, peeked, srcRC, capW, flusher)
	}

	finished := time.Now()
	firstDelta := timed.firstDelta
	if firstDelta.IsZero() {
		firstDelta = finished
	}
	ttftMs := firstDelta.Sub(attStart).Milliseconds()
	genMs := finished.Sub(firstDelta).Milliseconds()
	attempts[len(attempts)-1].TTFTMs = ttftMs
	attempts[len(attempts)-1].LatencyMs = ttftMs
	totalDur := finished.Sub(exStarted).Milliseconds()
	if streamErr != nil {
		if e.health() != nil {
			e.health().RecordOutcome(cand.Plan.ID, true, totalDur)
			e.health().EnterPlanCooldown(cand.Plan.ID, "plan", "mid-stream drop", 10*time.Second, time.Now().UTC())
		}
		if e.Ledger != nil {
			_ = e.Ledger.Record(ledger.UsageEntry{
				Timestamp:  time.Now().UTC(),
				PlanID:     cand.Plan.ID,
				Model:      cand.Target.Model,
				Protocol:   cand.EndpointProto,
				RouteID:    sel.Route.ID,
				SessionKey: sel.Signals.SessionKey,
				Status:     502,
				ErrorKind:  "downstream_stream",
				TTFTMs:     ttftMs,
				DurationMs: totalDur,
				IsRetry:    *accountedAttempts > 0,
			})
		}
		writeMidStreamError(w, flusher, ingress.Protocol, secret.ScrubText(streamErr.Error(), scrubVals...))
		e.finalizeRecord(rec, cand, attempts, usage, http.StatusOK, w.Header(), preview.Bytes(), needBridge, scrubVals, exStarted, "downstream_stream")
		return
	}

	e.recordSuccess(sel, cand, usage, ttftMs, totalDur, genMs, accountedAttempts)
	e.finalizeRecord(rec, cand, attempts, usage, http.StatusOK, w.Header(), preview.Bytes(), needBridge, scrubVals, exStarted, "")
}

// ExecuteDecision executes an internal DecisionRequest (used by internal/decision and semanticTiering)
// through the exact same Router + Failover + Ledger + ExchangeSink pipeline.
func (e *Executor) ExecuteDecision(ctx context.Context, snap *config.Snapshot, routeID string, dreq *ir.DecisionRequest) (*ir.DecisionResponse, error) {
	if dreq == nil {
		return nil, fmt.Errorf("decision request is nil")
	}
	started := time.Now()
	model := dreq.Model
	if model == "" || model == routeID {
		if routeID != "" {
			model = routeID
		} else {
			model = "decision"
		}
	}
	rawBody, err := json.Marshal(dreq)
	if err != nil {
		return nil, err
	}
	sig := router.ExtractSignals("", config.ProtocolTypeSafeJev, config.FamilyDecision, model, "", rawBody)
	sig.RouteOverride = routeID

	sel, err := e.Router.Select(ctx, snap, sig)
	if err != nil || sel == nil || len(sel.Candidates) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no decision candidates available")
	}

	maxAttempts := sel.Route.Policy.EffectiveMaxAttempts()
	sel.Candidates = expandCredentialCandidates(sel.Candidates)
	if maxAttempts > len(sel.Candidates) {
		maxAttempts = len(sel.Candidates)
	}
	ingress := codec.IngressInfo{
		Protocol:       config.ProtocolTypeSafeJev,
		Family:         config.FamilyDecision,
		Action:         "systemone",
		RequestedModel: model,
		CleanPath:      "/v1/systemone",
	}

	var rec *shared.Request
	if e.Sink != nil && e.CaptureMode != CaptureOff {
		rec = e.Sink.Start(&shared.Request{
			StartedAt:  started,
			Kind:       "gateway",
			Method:     http.MethodPost,
			Scheme:     "http",
			Host:       "internal.crabbro",
			Path:       "/v1/systemone",
			URL:        "http://internal.crabbro/v1/systemone",
			ClientAddr: "127.0.0.1",
			Gateway: &shared.GatewayTrace{
				RouteID:         sel.Route.ID,
				Family:          config.FamilyDecision,
				IngressProtocol: config.ProtocolTypeSafeJev,
				RequestedModel:  model,
				TurnPhase:       sig.TurnPhase,
				RouteDecision:   sel.Decision,
			},
		})
		if e.CaptureMode == CapturePreview && rec != nil {
			rec.RequestBodyRef = e.Sink.StoreBody(rawBody)
			rec.RequestBodyBytes = int64(len(rawBody))
		}
	}

	var attempts []shared.GatewayAttempt
	accountedAttempts := 0
	var lastErr error
	upstreamAttempts := 0
	for _, cand := range sel.Candidates {
		if upstreamAttempts >= maxAttempts {
			break
		}
		if e.candidateCooling(cand) {
			continue
		}
		auth := e.authenticator(cand)
		if !auth.DecisionAllowed() {
			lastErr = errors.New("SIWC session identity verification is unavailable")
			continue
		}
		attStart := time.Now()
		secErr := auth.Load(ctx)
		if secErr != nil {
			lastErr = secErr
			continue
		}
		scrubVals := auth.ScrubValues()
		needBridge := cand.EndpointProto != config.ProtocolTypeSafeJev
		if needBridge && e.Bridge == nil {
			lastErr = fmt.Errorf("bridge unavailable for %s", cand.EndpointProto)
			continue
		}

		var upBody []byte
		var bridgeState any
		if !needBridge {
			upBody, err = codec.PatchModelAndExtraBody(rawBody, cand.EndpointProto, cand.Target.Model, cand.ModelSpec.ExtraBody)
		} else {
			upBody, bridgeState, err = e.Bridge.BuildUpstreamBody(cand, ingress, rawBody)
		}
		if err != nil {
			lastErr = err
			continue
		}
		upURL, err := codec.BuildUpstreamURL(cand.Endpoint.BaseURL, cand.EndpointProto, ingress.Action, cand.Target.Model, "", false)
		if err != nil {
			lastErr = err
			continue
		}
		tr, err := e.Transports.ForProxy(cand.Plan.ProxyURL)
		if err != nil {
			lastErr = err
			continue
		}

		reqCtx, cancelReq := context.WithCancel(ctx)
		timer := time.AfterFunc(cand.Plan.Timeout.TTFTDuration(), cancelReq)
		upReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, upURL, bytes.NewReader(upBody))
		upReq.Header.Set("Content-Type", "application/json")
		codec.InjectUpstreamAuth(upReq.Header, cand.Endpoint, cand.EndpointProto, "")
		if upReq.Header.Get("User-Agent") == "" {
			upReq.Header.Set("User-Agent", "crabbro-gateway/1.0")
		}
		upBody, err = transform.Request(upReq, upBody, cand.Endpoint.AuthHeader, cand.Endpoint.RequestTransform, cand.ModelSpec.RequestTransform)
		if err != nil {
			timer.Stop()
			cancelReq()
			lastErr = err
			continue
		}
		upBody, err = auth.Prepare(upReq, upBody)
		if err != nil {
			timer.Stop()
			cancelReq()
			lastErr = err
			continue
		}

		if e.health() != nil {
			e.health().AcquireInFlight(cand.Plan.ID)
		}
		upstreamAttempts++
		resp, err := tr.RoundTrip(upReq)
		timer.Stop()
		if e.health() != nil {
			e.health().ReleaseInFlight(cand.Plan.ID)
		}
		latMs := time.Since(attStart).Milliseconds()
		if err != nil {
			cancelReq()
			fc := ClassifyHTTPFailure(0, []byte(err.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, latMs, sel.Route.ID, "", 0, &accountedAttempts)
			attempts = append(attempts, shared.GatewayAttempt{
				Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Model: cand.Target.Model, Protocol: cand.EndpointProto,
				Status: http.StatusBadGateway, ErrorKind: fc.Kind, ErrorMessage: secret.ScrubText(err.Error(), scrubVals...),
				LatencyMs: latMs, CooldownApplied: fc.CooldownLabel,
			})
			lastErr = err
			continue
		}

		respBytes, readErr := transform.ReadResponse(resp.Body)
		if readErr == nil && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header, respBytes, readErr = transform.Response(resp.Header, respBytes, cand.Endpoint.AuthHeader, cand.Endpoint.ResponseTransform, cand.ModelSpec.ResponseTransform)
			resp.Header = scrub.ScrubHeaders(resp.Header, scrubVals, cand.Endpoint.AuthHeader)
			respBytes = []byte(scrub.ScrubText(string(respBytes), scrubVals...))
		}
		resp.Body.Close()
		cancelReq()
		if readErr != nil {
			fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(readErr.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, time.Since(attStart).Milliseconds(), sel.Route.ID, "", http.StatusBadGateway, &accountedAttempts)
			lastErr = fmt.Errorf("%s", secret.ScrubText(readErr.Error(), scrubVals...))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			fc := ClassifyHTTPFailure(resp.StatusCode, respBytes, resp.Header, cand.Plan.ID, e.health(), cand.CredentialRef)
			e.applyFailure(cand, fc, latMs, sel.Route.ID, "", resp.StatusCode, &accountedAttempts)
			attempts = append(attempts, shared.GatewayAttempt{
				Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Model: cand.Target.Model, Protocol: cand.EndpointProto,
				Status: resp.StatusCode, ErrorKind: fc.Kind, ErrorMessage: secret.ScrubText(fc.Summary, scrubVals...),
				LatencyMs: latMs, CooldownApplied: fc.CooldownLabel,
			})
			lastErr = fmt.Errorf("decision backend status %d", resp.StatusCode)
			if !fc.Retryable {
				break
			}
			continue
		}

		var dresp *ir.DecisionResponse
		if !needBridge {
			var parsed ir.DecisionResponse
			if uErr := json.Unmarshal(respBytes, &parsed); uErr != nil {
				fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(uErr.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
				e.applyFailure(cand, fc, time.Since(attStart).Milliseconds(), sel.Route.ID, "", http.StatusBadGateway, &accountedAttempts)
				lastErr = uErr
				continue
			}
			parsed.Calibrated = true
			parsed.Mode = "native"
			parsed.Usage = codec.ExtractUsageJSON(config.ProtocolTypeSafeJev, respBytes)
			dresp = &parsed
		} else {
			dresp, err = e.Bridge.DecodeDecisionResponse(cand, bridgeState, respBytes)
			if err != nil {
				fc := ClassifyHTTPFailure(http.StatusBadGateway, []byte(err.Error()), nil, cand.Plan.ID, e.health(), cand.CredentialRef)
				e.applyFailure(cand, fc, time.Since(attStart).Milliseconds(), sel.Route.ID, "", http.StatusBadGateway, &accountedAttempts)
				lastErr = err
				continue
			}
			dresp.Calibrated = cand.Calibrated
			dresp.Mode = cand.DecisionMode
		}

		attempts = append(attempts, shared.GatewayAttempt{
			Plan: cand.Plan.ID, CredentialRef: cand.CredentialRef, Model: cand.Target.Model, Protocol: cand.EndpointProto,
			Status: resp.StatusCode, LatencyMs: latMs, TTFTMs: latMs,
		})
		e.recordSuccess(sel, cand, dresp.Usage, latMs, time.Since(started).Milliseconds(), 0, &accountedAttempts)
		e.finalizeRecord(rec, cand, attempts, dresp.Usage, resp.StatusCode, resp.Header, respBytes, needBridge, scrubVals, started, "")
		return dresp, nil
	}

	if rec != nil {
		rec.Gateway.Attempts = attempts
		e.finalizeRecord(rec, router.Candidate{}, attempts, shared.TokenUsage{}, http.StatusBadGateway, nil, nil, false, nil, started, "decision_failed")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("decision invocation failed")
	}
	return nil, lastErr
}

// peekFirstSSE reads from r up to maxSSEPeekBytes skipping blank lines,
// SSE comments (:...), and benign ping events until the first substantive event arrives.
// It returns rest (the bufio.Reader) so bytes read ahead by bufio are never dropped.
func peekFirstSSE(r io.Reader, proto string) (peeked []byte, rest io.Reader, errCode, errMsg string, hasErr, emptyEOF bool) {
	br := bufio.NewReader(r)
	var rawBuf bytes.Buffer
	var evName string
	var dataBuf bytes.Buffer

	for rawBuf.Len() < maxSSEPeekBytes {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			rawBuf.Write(line)
			trimmed := strings.TrimRight(string(line), "\r\n")
			switch {
			case trimmed == "":
				if dataBuf.Len() > 0 {
					c, m, isErr := codec.DetectSSEError(proto, evName, dataBuf.Bytes())
					if isErr {
						return rawBuf.Bytes(), br, c, m, true, false
					}
					// Ignore ping events and keep peeking until substantive data or end.
					if evName == "ping" || bytes.Contains(dataBuf.Bytes(), []byte(`"type":"ping"`)) {
						evName = ""
						dataBuf.Reset()
						continue
					}
					return rawBuf.Bytes(), br, "", "", false, false
				}
				evName = ""
			case strings.HasPrefix(trimmed, ":"):
				// comment / keepalive
			case strings.HasPrefix(trimmed, "event:"):
				evName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				d := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(d)
			}
		}
		if err != nil {
			if dataBuf.Len() > 0 {
				c, m, isErr := codec.DetectSSEError(proto, evName, dataBuf.Bytes())
				if isErr {
					return rawBuf.Bytes(), br, c, m, true, false
				}
				return rawBuf.Bytes(), br, "", "", false, false
			}
			if errors.Is(err, io.EOF) && rawBuf.Len() == 0 {
				return nil, br, "", "", false, true
			}
			if rawBuf.Len() > 0 {
				return rawBuf.Bytes(), br, "", "", false, false
			}
			return nil, br, "stream_read_error", err.Error(), true, false
		}
	}
	return rawBuf.Bytes(), br, "", "", false, false
}

func (e *Executor) health() *router.HealthStore {
	if e == nil || e.Router == nil {
		return nil
	}
	return e.Router.Health
}

func (e *Executor) applyFailure(cand router.Candidate, fc FailureClass, latencyMs int64, routeID, sessionKey string, status int, accountedAttempts *int) {
	now := time.Now().UTC()
	if e.health() != nil {
		e.health().RecordOutcome(cand.Plan.ID, true, latencyMs)
		switch fc.CooldownScope {
		case "key":
			e.health().EnterKeyCooldown(cand.Plan.ID, cand.CredentialRef, fc.CooldownDur, now)
		case "plan":
			e.health().EnterPlanCooldown(cand.Plan.ID, "plan", fc.Summary, fc.CooldownDur, now)
		case "model":
			e.health().EnterModelCooldown(cand.Plan.ID, cand.Target.Model, fc.Summary, fc.CooldownDur, now)
		}
	}
	if e.AutoResetSignal != nil && (fc.Kind == ErrKindQuotaExhausted || fc.Kind == ErrKindRateLimit) {
		e.AutoResetSignal(cand.Plan.ID)
	}
	if e.Ledger != nil {
		if fc.Kind == ErrKindRateLimit && e.Ledger.Calibrator != nil {
			e.Ledger.Calibrator.NoteRateLimit(cand.Plan.ID, now)
		}
		_ = e.Ledger.Record(ledger.UsageEntry{
			Timestamp:  now,
			PlanID:     cand.Plan.ID,
			Model:      cand.Target.Model,
			Protocol:   cand.EndpointProto,
			RouteID:    routeID,
			SessionKey: sessionKey,
			Status:     status,
			ErrorKind:  fc.Kind,
			DurationMs: latencyMs,
			IsRetry:    accountedAttempts != nil && *accountedAttempts > 0,
		})
	}
	if accountedAttempts != nil {
		*accountedAttempts++
	}
}

func (e *Executor) recordSuccess(sel *router.Selection, cand router.Candidate, usage shared.TokenUsage, ttftMs, totalMs, genMs int64, accountedAttempts *int) {
	now := time.Now().UTC()
	if e.health() != nil {
		e.health().RecordOutcome(cand.Plan.ID, false, totalMs)
		e.health().ResetKeyBackoff(cand.Plan.ID, cand.CredentialRef)
	}
	if e.Ledger != nil {
		if e.Ledger.Calibrator != nil {
			e.Ledger.Calibrator.NoteSuccess(cand.Plan.ID)
		}
		_ = e.Ledger.Record(ledger.UsageEntry{
			Timestamp:        now,
			PlanID:           cand.Plan.ID,
			Model:            cand.Target.Model,
			Protocol:         cand.EndpointProto,
			RouteID:          sel.Route.ID,
			SessionKey:       sel.Signals.SessionKey,
			Status:           200,
			InputTokens:      usage.InputTokens,
			OutputTokens:     usage.OutputTokens,
			CacheReadTokens:  usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
			ReasoningTokens:  usage.ReasoningTokens,
			TotalTokens:      usage.Total(),
			TTFTMs:           ttftMs,
			DurationMs:       totalMs,
			GenMs:            genMs,
			IsRetry:          accountedAttempts != nil && *accountedAttempts > 0,
		})
	}
	if e.Router != nil && e.Router.Sessions != nil && sel.Signals.SessionKey != "" {
		warm := usage.CacheReadTokens + usage.InputTokens
		if warm <= 0 {
			warm = int64(sel.Signals.EstimatedInputTokens)
		}
		e.Router.Sessions.Update(router.SessionCacheEntry{
			SessionKey:       sel.Signals.SessionKey,
			LastPlan:         cand.Plan.ID,
			LastModel:        cand.Target.Model,
			WarmPrefixTokens: warm,
			LastSuccessAt:    now,
		})
	}
}

func (e *Executor) startRecord(r *http.Request, snap *config.Snapshot, ingress codec.IngressInfo, sig router.Signals, rawBody []byte, started time.Time) *shared.Request {
	if e.Sink == nil || e.CaptureMode == CaptureOff {
		return nil
	}
	values := siwcValues(snap, e.Secrets)
	scrubbedURL := secret.ScrubText(codec.ScrubbedDisplayURL(r), values...)
	parsed, _ := url.Parse(scrubbedURL)
	cleanHdrs := secret.ScrubHeaders(secret.StripAuthHeaders(r.Header), values)
	hdrList, flatHdrs := flattenHeaders(cleanHdrs)

	path := ingress.CleanPath
	if parsed != nil && parsed.Path != "" {
		path = parsed.Path
	}
	rec := &shared.Request{
		StartedAt:         started,
		Kind:              "gateway",
		Method:            r.Method,
		Scheme:            "http",
		Host:              secret.ScrubText(r.Host, values...),
		Path:              secret.ScrubText(path, values...),
		Query:             secret.ScrubText(ingress.CleanQuery, values...),
		URL:               scrubbedURL,
		OriginalURL:       scrubbedURL,
		EffectiveURL:      scrubbedURL,
		ClientAddr:        r.RemoteAddr,
		RequestHeaders:    flatHdrs,
		RequestHeaderList: hdrList,
		RequestBodyBytes:  int64(len(rawBody)),
		Gateway: &shared.GatewayTrace{
			Profile:         ingress.Profile,
			Family:          ingress.Family,
			IngressProtocol: ingress.Protocol,
			RequestedModel:  secret.ScrubText(ingress.RequestedModel, values...),
			TurnPhase:       sig.TurnPhase,
			SessionKey:      secret.ScrubText(sig.SessionKey, values...),
		},
	}
	rec = e.Sink.Start(rec)
	if e.CaptureMode == CapturePreview && len(rawBody) > 0 {
		var secretVals []string
		if e.Secrets != nil {
			secretVals = e.Secrets.AllValues()
			secretVals = append(secretVals, values...)
		}
		scrubbedBody := []byte(secret.ScrubText(string(rawBody), secretVals...))
		if len(scrubbedBody) > shared.MaxBodySize {
			scrubbedBody = scrubbedBody[:shared.MaxBodySize]
			rec.RequestBodyTruncated = true
		}
		rec.RequestBodyRef = e.Sink.StoreBody(scrubbedBody)
	}
	e.Sink.Emit(shared.EventRequestStarted, rec.ID, map[string]any{
		"method": rec.Method, "url": rec.URL, "host": rec.Host, "kind": "gateway",
	})
	e.Sink.Emit(shared.EventRequestHeaders, rec.ID, nil)
	return rec
}

func (e *Executor) finalizeRecord(
	rec *shared.Request,
	cand router.Candidate,
	attempts []shared.GatewayAttempt,
	usage shared.TokenUsage,
	status int,
	respHdr http.Header,
	respBody []byte,
	translated bool,
	scrubVals []string,
	started time.Time,
	failureStage string,
) {
	if rec == nil || e.Sink == nil {
		return
	}
	now := time.Now()
	rec.Status = status
	rec.CompletedAt = &now
	rec.DurationMs = now.Sub(started).Milliseconds()
	rec.FailureStage = failureStage

	cleanRespHdr := secret.ScrubHeaders(respHdr, scrubVals)
	rList, rFlat := flattenHeaders(cleanRespHdr)
	rec.ResponseHeaderList = rList
	rec.ResponseHeaders = rFlat

	if rec.Gateway != nil {
		if cand.Plan != nil {
			rec.Gateway.ChosenPlan = cand.Plan.ID
			rec.Gateway.ChosenModel = cand.Target.Model
			rec.Gateway.UpstreamProtocol = cand.EndpointProto
			rec.Gateway.Translated = translated
			if cand.DecisionMode != "" {
				rec.Gateway.DecisionMode = cand.DecisionMode
				cal := cand.Calibrated
				rec.Gateway.Calibrated = &cal
			}
		}
		rec.Gateway.Attempts = attempts
		if usage.Total() > 0 {
			uCopy := usage
			rec.Gateway.Usage = &uCopy
		}
	}

	if e.CaptureMode == CapturePreview && len(respBody) > 0 {
		scrubbed := []byte(secret.ScrubText(string(respBody), scrubVals...))
		if len(scrubbed) > shared.MaxBodySize {
			scrubbed = scrubbed[:shared.MaxBodySize]
			rec.ResponseBodyTruncated = true
		}
		rec.ResponseBodyBytes = int64(len(respBody))
		rec.ResponseBodyRef = e.Sink.StoreBody(scrubbed)
		e.Sink.Emit(shared.EventResponseBodyAvailable, rec.ID, nil)
	}

	e.Sink.Emit(shared.EventResponseHeaders, rec.ID, nil)
	if failureStage != "" {
		e.Sink.Emit(shared.EventRequestFailed, rec.ID, map[string]any{"status": status, "stage": failureStage})
	} else {
		e.Sink.Emit(shared.EventResponseCompleted, rec.ID, map[string]any{"status": status})
	}
	e.Sink.Complete(rec)
}

func (e *Executor) writeFailure(w http.ResponseWriter, rec *shared.Request, proto string, status int, code, msg string, started time.Time) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    code,
			"code":    code,
			"message": msg,
		},
	})
	if proto == config.ProtocolAnthropic {
		payload, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": code, "message": msg},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	e.finalizeRecord(rec, router.Candidate{}, nil, shared.TokenUsage{}, status, w.Header(), payload, false, nil, started, "gateway")
}

func (e *Executor) writeUpstreamError(w http.ResponseWriter, rec *shared.Request, attempts []shared.GatewayAttempt, status int, hdr http.Header, body []byte, started time.Time) {
	copyResponseHeaders(w.Header(), hdr)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	e.finalizeRecord(rec, router.Candidate{}, attempts, shared.TokenUsage{}, status, hdr, body, false, nil, started, "upstream")
}

func setTraceHeaders(h http.Header, routeID string, cand router.Candidate, attemptCount int, translated bool) {
	h.Set("X-Crabbro-Route", routeID)
	if cand.Plan != nil {
		h.Set("X-Crabbro-Plan", cand.Plan.ID)
	}
	h.Set("X-Crabbro-Model", cand.Target.Model)
	h.Set("X-Crabbro-Attempt", strconv.Itoa(attemptCount))
	if translated {
		h.Set("X-Crabbro-Translated", "1")
	}
	if cand.DecisionMode != "" {
		h.Set("X-Crabbro-Decision-Mode", cand.DecisionMode)
	}
}

func writeMidStreamError(w http.ResponseWriter, f http.Flusher, proto, msg string) {
	var frame string
	switch proto {
	case config.ProtocolAnthropic:
		b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}})
		frame = "event: error\ndata: " + string(b) + "\n\n"
	default:
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_stream_error"}})
		frame = "data: " + string(b) + "\n\ndata: [DONE]\n\n"
	}
	_, _ = io.WriteString(w, frame)
	if f != nil {
		f.Flush()
	}
}

func stripProxyHopHeaders(h http.Header) {
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "x-crabbro-decision-images" || lk == "connection" || lk == "keep-alive" || lk == "transfer-encoding" || lk == "upgrade" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func flattenHeaders(h http.Header) ([]shared.HeaderField, shared.Headers) {
	flat := shared.Headers{}
	list := make([]shared.HeaderField, 0, len(h))
	for k, vs := range h {
		cp := append([]string(nil), vs...)
		list = append(list, shared.HeaderField{Name: k, Values: cp})
		if len(cp) > 0 {
			flat[k] = strings.Join(cp, ", ")
		}
	}
	return list, flat
}

type streamTimingWriter struct {
	w          io.Writer
	tap        *codec.StreamDeltaTap
	firstDelta time.Time
}

func (s *streamTimingWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if s.firstDelta.IsZero() && n > 0 {
		written := time.Now()
		_, _ = s.tap.Write(p[:n])
		if s.tap.Found() {
			s.firstDelta = written
		}
	}
	return n, err
}

type streamSinkWriter struct {
	w       io.Writer
	f       http.Flusher
	tap     *codec.UsageTap
	sigTap  *codec.SignatureStreamTap
	preview *bytes.Buffer
}

func (s *streamSinkWriter) Write(p []byte) (int, error) {
	if s.tap != nil {
		_, _ = s.tap.Write(p)
	}
	if s.sigTap != nil {
		_, _ = s.sigTap.Write(p)
	}
	if s.preview != nil && s.preview.Len() < shared.MaxBodySize {
		rem := shared.MaxBodySize - s.preview.Len()
		if len(p) <= rem {
			s.preview.Write(p)
		} else {
			s.preview.Write(p[:rem])
		}
	}
	n, err := s.w.Write(p)
	if s.f != nil {
		s.f.Flush()
	}
	return n, err
}

type limitedPreviewWriter struct {
	w       io.Writer
	preview *bytes.Buffer
}

func (l *limitedPreviewWriter) Write(p []byte) (int, error) {
	if l.preview != nil && l.preview.Len() < shared.MaxBodySize {
		rem := shared.MaxBodySize - l.preview.Len()
		if len(p) <= rem {
			l.preview.Write(p)
		} else {
			l.preview.Write(p[:rem])
		}
	}
	return l.w.Write(p)
}

func expandCredentialCandidates(candidates []router.Candidate) []router.Candidate {
	var out []router.Candidate
	seen := make(map[string]bool)
	for _, cand := range candidates {
		refs := cand.CredentialRefs
		if len(refs) == 0 {
			refs = cand.Plan.CredentialRefs()
		}
		for _, ref := range refs {
			key := cand.Plan.ID + "\x00" + cand.Target.Model + "\x00" + cand.EndpointProto + "\x00" + ref
			if seen[key] {
				continue
			}
			seen[key] = true
			c := cand
			c.CredentialRef = ref
			out = append(out, c)
		}
	}
	return out
}

func (e *Executor) candidateCooling(cand router.Candidate) bool {
	h := e.health()
	if h == nil {
		return false
	}
	now := time.Now().UTC()
	planCooling, _, _ := h.IsPlanCooling(cand.Plan.ID, now)
	return planCooling || h.IsModelCooling(cand.Plan.ID, cand.Target.Model, now) || h.IsKeyCooling(cand.Plan.ID, cand.CredentialRef, now)
}
