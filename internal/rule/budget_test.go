package rule

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

// slowRuntime answers every plugin call after a fixed delay.
type slowRuntime struct {
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func (s *slowRuntime) Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-time.After(s.delay):
		return InvokeResult{Effect: "observe", Match: true}, nil
	case <-ctx.Done():
		return InvokeResult{}, ctx.Err()
	}
}
func (s *slowRuntime) Secret(string, string, string) (string, error) { return "", fmt.Errorf("none") }
func (s *slowRuntime) HasInstance(string, string) bool               { return true }

func stages(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += fmt.Sprintf("    - uses: slow.wait\n      timeoutMs: 400\n      continueOnError: true\n")
	}
	return out
}

var reqFor = &shared.Request{Host: "example.com", Method: "GET", Path: "/"}

// Five stages that each take their full allowance must not add up past the
// deadline the caller gave the rule.
func TestStagesShareOneDeadline(t *testing.T) {
	src := "version: 1\nid: slow\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n" + stages(5)
	e := NewRuleEngine()
	e.Runtime = &slowRuntime{delay: time.Second}
	start := time.Now()
	res := e.Evaluate(src, reqFor, "", 600*time.Millisecond)
	took := time.Since(start)
	if took > 900*time.Millisecond {
		t.Errorf("rule took %v with a 600ms deadline", took)
	}
	late := 0
	for _, s := range res.Stages {
		if s.Error == "rule evaluation budget exceeded" {
			late++
		}
	}
	if late == 0 {
		t.Errorf("no stage reported the exhausted budget: %+v", res.Stages)
	}
}

// The document's own timeout is the stage default, and it applies.
func TestDocumentTimeoutIsTheStageDefault(t *testing.T) {
	src := "version: 1\nid: slow\ntimeout: 1s\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n    - uses: slow.wait\n"
	e := NewRuleEngine()
	e.Runtime = &slowRuntime{delay: 700 * time.Millisecond}
	res := e.Evaluate(src, reqFor, "", 2*time.Second)
	if res.Error != "" || !res.Matched {
		t.Errorf("a 700ms stage under timeout: 1s failed: %q matched=%v", res.Error, res.Matched)
	}
}

// parallel waits up to its timeout, traces what did not finish as failed,
// and never fails the rule. Run with -race: an abandoned child must not
// write into the block's result.
func TestParallelAbandonsSlowChildrenWithoutRacing(t *testing.T) {
	src := "version: 1\nid: par\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n    - parallel:\n        timeoutMs: 100\n        stages:\n          - uses: slow.wait\n          - uses: crabbro.log\n            with:\n              message: hi\n"
	e := NewRuleEngine()
	e.Runtime = &slowRuntime{delay: time.Second}
	start := time.Now()
	res := e.Evaluate(src, reqFor, "", time.Second)
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Errorf("parallel took %v with timeoutMs 100", took)
	}
	if res.Error != "" {
		t.Errorf("parallel failed the rule: %q", res.Error)
	}
	// The slow child ends at its deadline either way: the block's own
	// timeout or the plugin call's. Either is traced as its failure, once.
	failed := 0
	for _, s := range res.Stages {
		if s.Error != "" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("want the slow child traced as failed once, got %d: %+v", failed, res.Stages)
	}
	time.Sleep(150 * time.Millisecond)
}

// A script is parsed and checked once, however many exchanges evaluate it.
func TestScriptIsCompiledOnce(t *testing.T) {
	src := "version: 1\nid: cached\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n    - uses: crabbro.tag\n      with:\n        tags: [x]\n"
	e := NewRuleEngine()
	first := compileScript(src)
	for i := 0; i < 50; i++ {
		e.Evaluate(src, reqFor, "", time.Second)
	}
	if compileScript(src) != first {
		t.Error("the script was compiled again")
	}
}
