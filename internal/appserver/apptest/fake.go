package apptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// A test binary doubles as a fake app-server: when ScenarioEnv names a
// scenario, the package's TestMain calls Run instead of running tests, and
// tests spawn os.Args[0] so the client talks to a real subprocess over real
// pipes. The fake validates requests with literal protocol strings so a
// wrong constant in the client cannot pass by matching itself.
const (
	// ScenarioEnv selects the fake's behavior.
	ScenarioEnv = "COUNTERPOINT_FAKE_APPSERVER"
	// StateEnv names a file where issued thread ids persist across runs;
	// received interrupts are appended to StateEnv + ".events".
	StateEnv = "COUNTERPOINT_FAKE_STATE"

	// Bounds mirrored from the client so scenarios can exceed them.
	maxMessageSize = 16 << 20
	outboxSize     = 64
	maxWarnings    = 32
)

// Main runs the fake when ScenarioEnv is set and returns true, so a
// package's TestMain can hand off to it before running tests.
func Main() bool {
	scenario := os.Getenv(ScenarioEnv)
	if scenario == "" {
		return false
	}
	os.Exit(Run(scenario, os.Getenv(StateEnv)))
	return true
}

// Wire shapes the fake needs, kept independent of the client's types.
type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type initializeParams struct {
	ClientInfo struct {
		Name string `json:"name"`
	} `json:"clientInfo"`
}

type threadStartParams struct {
	Cwd            string `json:"cwd"`
	Sandbox        string `json:"sandbox"`
	ApprovalPolicy string `json:"approvalPolicy"`
}

type threadResumeParams struct {
	ThreadID       string `json:"threadId"`
	Cwd            string `json:"cwd"`
	Sandbox        string `json:"sandbox"`
	ApprovalPolicy string `json:"approvalPolicy"`
}

type reviewStartParams struct {
	ThreadID string `json:"threadId"`
	Target   struct {
		Type         string `json:"type"`
		Instructions string `json:"instructions"`
	} `json:"target"`
	Delivery string `json:"delivery"`
}

type turnInterruptParams struct {
	TurnID string `json:"turnId"`
}

type fakeServer struct {
	scenario  string
	statePath string

	writeMu sync.Mutex
	out     *bufio.Writer

	mu          sync.Mutex
	initialized bool
	threads     map[string]bool
	nextThread  int
	nextTurn    int
	nextSrvID   int
	srvPending  map[string]chan envelope
	interrupts  map[string]chan struct{}
	heldStart   *envelope // reorder scenario: first thread/start held back
}

// Run serves the fake app-server on stdio until stdin closes.
func Run(scenario, statePath string) int {
	fmt.Fprintln(os.Stderr, "fake app-server starting: diagnostics on stderr must never reach stdout")
	f := &fakeServer{
		scenario:   scenario,
		statePath:  statePath,
		out:        bufio.NewWriter(os.Stdout),
		threads:    map[string]bool{},
		srvPending: map[string]chan envelope{},
		interrupts: map[string]chan struct{}{},
	}
	f.loadThreads()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessageSize)
	for scanner.Scan() {
		var env envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			fmt.Fprintln(os.Stderr, "fake: bad line:", err)
			continue
		}
		f.handle(&env)
	}
	if scenario == "linger" {
		time.Sleep(time.Minute)
	}
	return 0
}

func (f *fakeServer) loadThreads() {
	if f.statePath == "" {
		return
	}
	data, err := os.ReadFile(f.statePath)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(data)) {
		f.threads[id] = true
		f.nextThread++
	}
}

// recordEvent appends an observable event beside the state file so tests
// can verify requests the fake received.
func (f *fakeServer) recordEvent(event string) {
	if f.statePath == "" {
		return
	}
	file, err := os.OpenFile(f.statePath+".events", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(event + "\n")
	_ = file.Close()
}

func (f *fakeServer) saveThreads() {
	if f.statePath == "" {
		return
	}
	ids := make([]string, 0, len(f.threads))
	for id := range f.threads {
		ids = append(ids, id)
	}
	_ = os.WriteFile(f.statePath, []byte(strings.Join(ids, "\n")), 0o600)
}

func (f *fakeServer) send(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.out.Write(data)
	_ = f.out.WriteByte('\n')
	_ = f.out.Flush()
}

func (f *fakeServer) respond(id json.RawMessage, result any) {
	f.send(map[string]any{"id": id, "result": result})
}

func (f *fakeServer) fail(id json.RawMessage, code int, msg string) {
	f.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeServer) notify(method string, params any) {
	f.send(map[string]any{"method": method, "params": params})
}

func (f *fakeServer) handle(env *envelope) {
	// Responses to our server-originated requests carry our string ids.
	if env.Method == "" && len(env.ID) > 0 {
		var id string
		if json.Unmarshal(env.ID, &id) == nil {
			f.mu.Lock()
			ch, ok := f.srvPending[id]
			delete(f.srvPending, id)
			f.mu.Unlock()
			if ok {
				ch <- *env
			}
		}
		return
	}

	switch env.Method {
	case "initialize":
		if f.scenario == "bad-init" {
			f.fail(env.ID, -32000, "initialize rejected")
			return
		}
		var p initializeParams
		if json.Unmarshal(env.Params, &p) != nil || p.ClientInfo.Name == "" {
			f.fail(env.ID, -32602, "initialize requires clientInfo.name")
			return
		}
		if f.scenario == "bad-init-shape" {
			f.respond(env.ID, map[string]any{"userAgent": "fake"})
			return
		}
		f.respond(env.ID, map[string]any{"userAgent": "fake", "codexHome": "/tmp/fake-codex", "platformFamily": "unix", "platformOs": "fake"})
	case "initialized":
		f.mu.Lock()
		f.initialized = true
		f.mu.Unlock()
	case "thread/start":
		f.handleThreadStart(env)
	case "thread/resume":
		f.handleThreadResume(env)
	case "review/start":
		f.handleReviewStart(env)
	case "test/ping":
		// Never answered: lets tests hold a call pending.
	case "turn/interrupt":
		var p turnInterruptParams
		_ = json.Unmarshal(env.Params, &p)
		f.mu.Lock()
		ch := f.interrupts[p.TurnID]
		f.mu.Unlock()
		f.recordEvent("interrupt:" + p.TurnID)
		if ch != nil {
			close(ch)
		}
		f.respond(env.ID, map[string]any{})
	default:
		f.fail(env.ID, -32601, "unknown method "+env.Method)
	}
}

func (f *fakeServer) requireInit(id json.RawMessage) bool {
	f.mu.Lock()
	ok := f.initialized
	f.mu.Unlock()
	if !ok {
		f.fail(id, -32002, "initialized notification not received before "+string(id))
	}
	return ok
}

func (f *fakeServer) threadResult(id, cwd string) map[string]any {
	return map[string]any{
		"thread":          map[string]any{"id": id, "cwd": cwd},
		"model":           "fake-model",
		"reasoningEffort": "xhigh",
		"cwd":             cwd,
		"modelProvider":   "fake",
		"approvalPolicy":  "never",
		"sandbox":         map[string]any{"type": "readOnly", "networkAccess": false},
	}
}

func (f *fakeServer) handleThreadStart(env *envelope) {
	if !f.requireInit(env.ID) {
		return
	}
	var p threadStartParams
	// Literal protocol values, deliberately not the package constants, so a
	// wrong constant in the client cannot pass by matching itself.
	if json.Unmarshal(env.Params, &p) != nil || p.Cwd == "" || p.Sandbox != "read-only" || p.ApprovalPolicy != "never" {
		f.fail(env.ID, -32602, fmt.Sprintf("thread/start requires cwd, sandbox read-only and approvalPolicy never; got %s", env.Params))
		return
	}
	f.mu.Lock()
	f.nextThread++
	id := fmt.Sprintf("thr_%d", f.nextThread)
	f.threads[id] = true
	f.saveThreads()
	held := f.heldStart
	if f.scenario == "reorder" && held == nil {
		f.heldStart = env
		f.mu.Unlock()
		return
	}
	f.heldStart = nil
	f.mu.Unlock()

	// reorder: answer the second request first, then the held first one.
	result := f.threadResult(id, p.Cwd)
	switch f.scenario {
	case "wrong-policy":
		result["sandbox"] = map[string]any{"type": "workspaceWrite", "networkAccess": true}
	case "wrong-cwd":
		result["cwd"] = "/somewhere/else"
	}
	f.respond(env.ID, result)
	if f.scenario == "flood" {
		// Wait until the client starts sending its next message, then stop
		// reading and ask questions faster than the blocked client can
		// answer them. A single raw read is safe here: the scanner has
		// consumed exactly the thread/start line and nothing else has been
		// sent yet.
		_, _ = os.Stdin.Read(make([]byte, 64*1024))
		for i := 0; i < 3*outboxSize; i++ {
			f.send(map[string]any{"id": fmt.Sprintf("flood-%d", i), "method": "item/commandExecution/requestApproval",
				"params": map[string]any{"threadId": id, "turnId": "turn_x", "itemId": "c", "startedAtMs": 1}})
		}
		time.Sleep(time.Hour)
	}
	if f.scenario == "deaf" {
		// Stop consuming stdin; the handler runs on the read loop. A sleep
		// rather than select{} so the runtime's deadlock detector does not
		// end the process.
		time.Sleep(time.Hour)
	}
	if held != nil {
		f.mu.Lock()
		f.nextThread++
		heldID := fmt.Sprintf("thr_%d", f.nextThread)
		f.threads[heldID] = true
		f.saveThreads()
		f.mu.Unlock()
		f.respond(held.ID, f.threadResult(heldID, p.Cwd))
	}
}

func (f *fakeServer) handleThreadResume(env *envelope) {
	if !f.requireInit(env.ID) {
		return
	}
	var p threadResumeParams
	if json.Unmarshal(env.Params, &p) != nil || p.Cwd == "" || p.Sandbox != "read-only" || p.ApprovalPolicy != "never" {
		f.fail(env.ID, -32602, "thread/resume requires cwd, read-only sandbox and never approval")
		return
	}
	f.mu.Lock()
	known := f.threads[p.ThreadID]
	f.mu.Unlock()
	if !known {
		f.fail(env.ID, -32602, "thread not found: "+p.ThreadID)
		return
	}
	returned := p.ThreadID
	if f.scenario == "resume-other-id" {
		returned = "thr_impostor"
	}
	f.respond(env.ID, f.threadResult(returned, p.Cwd))
}

func (f *fakeServer) handleReviewStart(env *envelope) {
	if !f.requireInit(env.ID) {
		return
	}
	var p reviewStartParams
	if json.Unmarshal(env.Params, &p) != nil || p.Delivery != "inline" || p.Target.Type != "custom" || p.Target.Instructions == "" {
		f.fail(env.ID, -32602, "review/start requires inline delivery and a custom target with instructions")
		return
	}
	f.mu.Lock()
	known := f.threads[p.ThreadID]
	f.nextTurn++
	turnID := fmt.Sprintf("turn_%d", f.nextTurn)
	interrupt := make(chan struct{})
	f.interrupts[turnID] = interrupt
	f.mu.Unlock()
	if !known {
		f.fail(env.ID, -32602, "thread not found: "+p.ThreadID)
		return
	}

	threadID := p.ThreadID
	reviewThreadID := threadID
	switch f.scenario {
	case "wrong-thread":
		threadID = "thr_detached"
		reviewThreadID = threadID
	case "no-review-thread":
		reviewThreadID = ""
	}
	turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}
	if f.scenario == "notify-first" {
		f.notify("turn/started", map[string]any{"threadId": threadID, "turn": turn})
		f.notify("item/agentMessage/delta", map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "m1", "delta": "early "})
	}
	switch f.scenario {
	case "stale-events":
		// Events that must not be attributed to the review: a completion
		// for another turn on the same thread before the real turn starts,
		// and an item with no turn id at all.
		f.notify("turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn_stale", "status": "completed",
			"items": []any{map[string]any{"id": "s", "type": "exitedReviewMode", "review": "STALE"}}}})
		f.notify("item/completed", map[string]any{"threadId": threadID, "turnId": "", "completedAtMs": 1,
			"item": map[string]any{"id": "e", "type": "exitedReviewMode", "review": "EMPTY ID"}})
	case "started-id-disagrees":
		f.notify("turn/started", map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn_early", "status": "inProgress", "items": []any{}}})
	}
	if f.scenario == "started-then-stall" {
		// The turn is running before the response exists. Hold the
		// response until the client interrupts the turn it learned about
		// from turn/started.
		f.notify("turn/started", map[string]any{"threadId": threadID, "turn": turn})
		go func() {
			<-interrupt
			f.notify("turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "interrupted", "items": []any{}}})
			f.fail(env.ID, -32000, "turn interrupted before the response was sent")
		}()
		return
	}
	f.respond(env.ID, map[string]any{"reviewThreadId": reviewThreadID, "turn": turn})
	go f.runTurn(threadID, turnID, p.Target.Instructions, interrupt)
}

func (f *fakeServer) runTurn(threadID, turnID, instructions string, interrupt chan struct{}) {
	turn := map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}
	if f.scenario != "notify-first" {
		f.notify("turn/started", map[string]any{"threadId": threadID, "turn": turn})
	}
	// Noise from another thread must be ignored by the client. It is sent
	// after the real events so that a client without filtering would
	// overwrite the review and finish on the wrong terminal status.
	noise := func() {
		f.notify("item/agentMessage/delta", map[string]any{"threadId": "thr_other", "turnId": "turn_other", "itemId": "x", "delta": "NOISE"})
		f.notify("item/completed", map[string]any{"threadId": "thr_other", "turnId": "turn_other", "completedAtMs": 1,
			"item": map[string]any{"id": "x", "type": "exitedReviewMode", "review": "NOISE REVIEW"}})
		f.notify("turn/completed", map[string]any{"threadId": "thr_other", "turn": map[string]any{"id": "turn_other", "status": "failed",
			"items": []any{}, "error": map[string]any{"message": "NOISE FAILURE"}}})
	}
	complete := func(status string, extra map[string]any) {
		noise()
		t := map[string]any{"id": turnID, "status": status, "items": []any{}}
		for k, v := range extra {
			t[k] = v
		}
		f.notify("turn/completed", map[string]any{"threadId": threadID, "turn": t})
	}
	delta := func(s string) {
		f.notify("item/agentMessage/delta", map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "m1", "delta": s})
	}
	message := func(text string) {
		f.notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "completedAtMs": 1,
			"item": map[string]any{"id": "m1", "type": "agentMessage", "text": text}})
	}
	reviewItem := func(text string) {
		f.notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "completedAtMs": 2,
			"item": map[string]any{"id": "r1", "type": "exitedReviewMode", "review": text}})
	}
	firstLine, _, _ := strings.Cut(instructions, "\n")
	reviewText := fmt.Sprintf("REVIEW for %s: %d instruction bytes; first line: %s", threadID, len(instructions), firstLine)

	switch f.scenario {
	case "fail":
		f.notify("error", map[string]any{"threadId": threadID, "turnId": turnID, "willRetry": false, "error": map[string]any{"message": "boom"}})
		complete("failed", map[string]any{"error": map[string]any{"message": "boom", "codexErrorInfo": "other"}})
	case "hang":
		<-interrupt
		complete("interrupted", nil)
	case "exit":
		os.Exit(3)
	case "huge":
		delta(strings.Repeat("x", maxMessageSize+1))
	case "large":
		delta(strings.Repeat("d", 100*1024))
		reviewItem(strings.Repeat("R", 200*1024))
		complete("completed", nil)
	case "no-review-item":
		message("first")
		message("second")
		complete("completed", nil)
	case "delta-only":
		delta("only ")
		delta("deltas")
		complete("completed", nil)
	case "items-in-completed":
		complete("completed", map[string]any{"items": []any{
			map[string]any{"id": "r1", "type": "exitedReviewMode", "review": "from completed items"},
		}})
	case "empty":
		complete("completed", nil)
	case "exit-after-complete":
		// A large final burst so unread bytes remain in the pipe at exit.
		reviewItem(reviewText + strings.Repeat("!", 256*1024))
		complete("completed", nil)
		os.Exit(0)
	case "aggregate-overflow":
		// Each message is valid on its own; together they exceed the bound.
		chunk := strings.Repeat("m", 1<<20)
		for i := 0; i < 17; i++ {
			message(chunk)
		}
		complete("completed", nil)
	case "warning-flood":
		// Far more requests than the warning bound, each with a huge id.
		// Replies are consumed by the read loop and ignored.
		hugeID := strings.Repeat("x", 4096)
		for i := 0; i < 10*maxWarnings; i++ {
			f.send(map[string]any{"id": fmt.Sprintf("wf-%d", i), "method": "item/commandExecution/requestApproval",
				"params": map[string]any{"threadId": threadID, "turnId": turnID, "itemId": hugeID, "startedAtMs": 1}})
		}
		reviewItem(reviewText)
		complete("completed", nil)
	case "approval":
		if msg := f.runApprovals(threadID, turnID); msg != "" {
			complete("failed", map[string]any{"error": map[string]any{"message": msg}})
			return
		}
		reviewItem(reviewText)
		complete("completed", nil)
	default:
		delta("Looking ")
		delta("at the diff")
		message("Looking at the diff")
		reviewItem(reviewText)
		complete("completed", nil)
	}
}

// runApprovals sends every kind of server-originated request and checks the
// client's answer. It returns a failure message, or "" when all are right.
func (f *fakeServer) runApprovals(threadID, turnID string) string {
	base := map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "c1", "startedAtMs": 1}
	with := func(kv map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	checks := []struct {
		method string
		params map[string]any
		want   func(env envelope) bool
	}{
		{"item/commandExecution/requestApproval", with(map[string]any{"command": "rm -rf /"}), wantField("decision", "decline")},
		{"item/fileChange/requestApproval", with(map[string]any{"reason": "wants to edit"}), wantField("decision", "decline")},
		{"item/permissions/requestApproval", with(map[string]any{"cwd": "/x", "permissions": map[string]any{}}), wantKey("permissions")},
		{"item/tool/requestUserInput", with(map[string]any{"isBlocking": true, "questions": []any{}}), wantKey("answers")},
		{"execCommandApproval", map[string]any{"conversationId": threadID, "callId": "1", "command": []string{"ls"}, "cwd": "/x", "parsedCmd": []any{}}, wantLegacyDenied},
		{"item/weird/request", with(nil), func(env envelope) bool { return env.Error != nil }},
	}
	for _, c := range checks {
		f.mu.Lock()
		f.nextSrvID++
		id := fmt.Sprintf("srv-%d", f.nextSrvID)
		ch := make(chan envelope, 1)
		f.srvPending[id] = ch
		f.mu.Unlock()
		f.send(map[string]any{"id": id, "method": c.method, "params": c.params})
		select {
		case env := <-ch:
			if !c.want(env) {
				return fmt.Sprintf("unexpected answer to %s: result=%s error=%v", c.method, env.Result, env.Error)
			}
		case <-time.After(5 * time.Second):
			return "no answer to " + c.method
		}
	}
	return ""
}

func wantField(key, value string) func(envelope) bool {
	return func(env envelope) bool {
		var m map[string]any
		return env.Error == nil && json.Unmarshal(env.Result, &m) == nil && m[key] == value
	}
}

// wantLegacyDenied checks the schema's denial shape for the legacy approval
// methods: {"decision": {"denied": {"rejection": "<non-empty>"}}}.
func wantLegacyDenied(env envelope) bool {
	var r struct {
		Decision struct {
			Denied *struct {
				Rejection string `json:"rejection"`
			} `json:"denied"`
		} `json:"decision"`
	}
	return env.Error == nil && json.Unmarshal(env.Result, &r) == nil && r.Decision.Denied != nil && r.Decision.Denied.Rejection != ""
}

func wantKey(key string) func(envelope) bool {
	return func(env envelope) bool {
		var m map[string]any
		if env.Error != nil || json.Unmarshal(env.Result, &m) != nil {
			return false
		}
		_, ok := m[key]
		return ok
	}
}
