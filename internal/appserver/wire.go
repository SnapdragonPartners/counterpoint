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
type tokenUsageNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	// Usage is a pointer so an absent or null tokenUsage is distinguishable
	// from one reporting zeros: absent means the app-server said nothing,
	// which must stay unknown rather than become a recorded zero.
	Usage *struct {
		Last  usageBreakdown `json:"last"`
		Total usageBreakdown `json:"total"`
	} `json:"tokenUsage"`
}

// negative reports whether any counter is below zero. Counts come from the
// child process, which is untrusted: a negative counter persisted with the
// round makes every later round of that workflow fail state validation, so
// it is refused here rather than carried inward.
func (b usageBreakdown) negative() bool {
	return b.InputTokens < 0 || b.CachedInputTokens < 0 || b.CacheWriteInputTokens < 0 ||
		b.OutputTokens < 0 || b.ReasoningOutputTokens < 0 || b.TotalTokens < 0
}

// usageBreakdown is TokenUsageBreakdown from the app-server schema.
type usageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
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
