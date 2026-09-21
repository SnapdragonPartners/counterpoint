// Package appserver speaks JSON-RPC over JSONL stdio to a codex app-server
// child process and implements the protocol subset in docs/SPEC.md: the
// initialize handshake, thread start and resume, an inline custom review on
// the persistent thread, turn completion, interruption, and bounded declines
// for every server-originated request.
//
// Wire types live in this file and never leave the package; the exported API
// in client.go uses domain types validated at the boundary.
package appserver

import (
	"encoding/json"
	"fmt"
)

// envelope is the superset of every JSONL message. A request has method and
// id, a notification has method only, a response has id with result or
// error. Server-originated requests look like requests and are answered.
type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ServerError    `json:"error,omitempty"`
}

// ServerError is a JSON-RPC error object returned by the app-server, as
// opposed to a transport, process, or policy failure. Callers that need to
// know the server answered use errors.As.
type ServerError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message)
}

// JSON-RPC error codes used when answering server-originated requests.
const (
	codeMethodNotFound = -32601
)

// Method names in the v2 app-server protocol used by this package.
const (
	methodInitialize   = "initialize"
	methodInitialized  = "initialized"
	methodThreadStart  = "thread/start"
	methodThreadResume = "thread/resume"
	// thread/unarchive restores an archived thread without loading it; it
	// needs the thread's writer, so it fails while another process holds
	// the thread. thread/name/set names a thread for the Codex UIs.
	methodThreadUnarchive = "thread/unarchive"
	methodThreadNameSet   = "thread/name/set"
	methodReviewStart     = "review/start"
	methodTurnInterrupt   = "turn/interrupt"

	notifyTurnStarted       = "turn/started"
	notifyTurnCompleted     = "turn/completed"
	notifyItemCompleted     = "item/completed"
	notifyTokenUsage        = "thread/tokenUsage/updated"
	notifyAgentMessageDelta = "item/agentMessage/delta"
	notifyError             = "error"

	requestCommandApproval     = "item/commandExecution/requestApproval"
	requestFileChangeApproval  = "item/fileChange/requestApproval"
	requestPermissions         = "item/permissions/requestApproval"
	requestUserInput           = "item/tool/requestUserInput"
	requestLegacyExecApproval  = "execCommandApproval"
	requestLegacyPatchApproval = "applyPatchApproval"
)

// Protocol values.
const (
	sandboxReadOnly             = "read-only"
	sandboxWorkspaceWrite       = "workspace-write"
	sandboxPolicyReadOnly       = "readOnly"
	sandboxPolicyWorkspaceWrite = "workspaceWrite"
	approvalNever               = "never"
	reviewDeliveryInline        = "inline"
	reviewTargetCustom          = "custom"
	decisionDecline             = "decline"
	legacyDecisionDenied        = "denied"

	turnStatusCompleted   = "completed"
	turnStatusFailed      = "failed"
	turnStatusInterrupted = "interrupted"

	itemTypeExitedReviewMode = "exitedReviewMode"
	itemTypeAgentMessage     = "agentMessage"
)

// Request and response parameter shapes, limited to the fields used.

type initializeParams struct {
	ClientInfo   clientInfo `json:"clientInfo"`
	Capabilities struct{}   `json:"capabilities"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
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

type threadUnarchiveParams struct {
	ThreadID string `json:"threadId"`
}

type threadNameSetParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

// initializeResponse carries the fields the schema requires; an
// incompatible server is detected by their absence.
type initializeResponse struct {
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
	UserAgent      string `json:"userAgent"`
}

// threadResponse covers both thread/start and thread/resume responses,
// including the effective policy the server reports back.
type threadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model           string          `json:"model"`
	ReasoningEffort *string         `json:"reasoningEffort"`
	ApprovalPolicy  json.RawMessage `json:"approvalPolicy"`
	Sandbox         sandboxPolicy   `json:"sandbox"`
	Cwd             string          `json:"cwd"`
}

// sandboxPolicy is the effective sandbox reported on thread responses. The
// workspace-write fields are absent on a read-only policy.
type sandboxPolicy struct {
	Type                string   `json:"type"`
	NetworkAccess       bool     `json:"networkAccess"`
	WritableRoots       []string `json:"writableRoots"`
	ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
	ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
}

type reviewStartParams struct {
	ThreadID string       `json:"threadId"`
	Target   reviewTarget `json:"target"`
	Delivery string       `json:"delivery"`
}

type reviewTarget struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type reviewStartResponse struct {
	ReviewThreadID string   `json:"reviewThreadId"`
	Turn           turnInfo `json:"turn"`
}

type turnInfo struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Error  *turnError   `json:"error"`
	Items  []threadItem `json:"items"`
}

type turnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// Notification parameter shapes.

type turnNotification struct {
	ThreadID string   `json:"threadId"`
	Turn     turnInfo `json:"turn"`
}

type itemNotification struct {
	ThreadID string     `json:"threadId"`
	TurnID   string     `json:"turnId"`
	Item     threadItem `json:"item"`
}

type threadItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text"`
	Review string `json:"review"`
}

type agentMessageDelta struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Delta    string `json:"delta"`
}

// tokenUsageNotification reports the thread's token usage during a turn.
// Last is the app-server's most recent report and Total is cumulative for
// the thread; the schema documents neither's scope, so both are carried
// through and the interpretation is recorded in docs/SPEC.md.
//
// Usage and the breakdowns inside it are pointers so that absent and null
// are distinguishable from a report of zeros at every level. The schema
// makes threadId, turnId and tokenUsage required, and last and total
// required within tokenUsage; decoding any of them into a value type would
// turn "the app-server said nothing" into "the turn cost nothing".
type tokenUsageNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Usage    *struct {
		Last  *usageBreakdown `json:"last"`
		Total *usageBreakdown `json:"total"`
	} `json:"tokenUsage"`
}

// valid reports whether the notification carries a usable report: both
// breakdowns present, every counter the schema requires present, and no
// counter negative. Counts come from the child process, which is untrusted:
// a negative counter persisted with the round makes every later round of
// that workflow fail state validation, and an invented zero is a cost
// reported for a turn nobody measured.
func (n tokenUsageNotification) valid() bool {
	return n.Usage != nil && n.Usage.Last.valid() && n.Usage.Total.valid()
}

// usageBreakdown is TokenUsageBreakdown from the app-server schema. Every
// counter the schema requires is a pointer so a missing one is refused
// rather than read as zero. cacheWriteInputTokens is the one field the
// schema makes optional, with a documented default of zero.
type usageBreakdown struct {
	InputTokens           *int64 `json:"inputTokens"`
	CachedInputTokens     *int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens *int64 `json:"cacheWriteInputTokens"`
	OutputTokens          *int64 `json:"outputTokens"`
	ReasoningOutputTokens *int64 `json:"reasoningOutputTokens"`
	TotalTokens           *int64 `json:"totalTokens"`
}

// valid reports whether every required counter is present and no counter is
// negative. A nil breakdown is invalid: the schema requires both.
func (b *usageBreakdown) valid() bool {
	if b == nil {
		return false
	}
	for _, c := range [...]*int64{
		b.InputTokens, b.CachedInputTokens, b.OutputTokens,
		b.ReasoningOutputTokens, b.TotalTokens,
	} {
		if c == nil || *c < 0 {
			return false
		}
	}
	return b.CacheWriteInputTokens == nil || *b.CacheWriteInputTokens >= 0
}

// resolve returns the counters, substituting the schema's documented zero
// default for an absent cacheWriteInputTokens. valid is the gate that keeps
// an absent required counter from being read as zero; resolve does not
// depend on having been called after it, because a nil dereference here
// would panic in the connection's reader goroutine and take the server
// down with it.
func (b *usageBreakdown) resolve() UsageBreakdown {
	if b == nil {
		return UsageBreakdown{}
	}
	at := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return UsageBreakdown{
		Input:      at(b.InputTokens),
		Cached:     at(b.CachedInputTokens),
		CacheWrite: at(b.CacheWriteInputTokens),
		Output:     at(b.OutputTokens),
		Reasoning:  at(b.ReasoningOutputTokens),
		Total:      at(b.TotalTokens),
	}
}

type errorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

// Server-originated request parameter shape: only the fields needed to
// describe the declined request in a warning.
type serverRequestParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
}
