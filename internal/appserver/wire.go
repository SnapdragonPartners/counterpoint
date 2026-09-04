// Package appserver speaks JSON-RPC over JSONL stdio to a codex app-server
// child process and implements the protocol subset in docs/MVP.md: the
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
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message)
}

// JSON-RPC error codes used when answering server-originated requests.
const (
	codeMethodNotFound = -32601
)

// Method names in the v2 app-server protocol used by this package.
const (
	methodInitialize    = "initialize"
	methodInitialized   = "initialized"
	methodThreadStart   = "thread/start"
	methodThreadResume  = "thread/resume"
	methodReviewStart   = "review/start"
	methodTurnInterrupt = "turn/interrupt"

	notifyTurnStarted       = "turn/started"
	notifyTurnCompleted     = "turn/completed"
	notifyItemCompleted     = "item/completed"
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
	sandboxReadOnly       = "read-only"
	sandboxPolicyReadOnly = "readOnly"
	approvalNever         = "never"
	reviewDeliveryInline  = "inline"
	reviewTargetCustom    = "custom"
	decisionDecline       = "decline"
	legacyDecisionDenied  = "denied"

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

// sandboxPolicy is the effective sandbox reported on thread responses.
type sandboxPolicy struct {
	Type          string `json:"type"`
	NetworkAccess bool   `json:"networkAccess"`
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
