// Package rpc implements gmcli's local control surface: newline-delimited
// JSON-RPC 2.0 over a unix domain socket. `gmcli serve` hosts the server;
// the bundled Go client (client.go), the gmtui Rust client, and any agent
// runtime that can speak NDJSON are consumers.
//
// Wire format, one JSON document per line:
//
//	-> {"jsonrpc":"2.0","id":1,"method":"chats.list","params":{"limit":20}}
//	<- {"jsonrpc":"2.0","id":1,"result":[...]}
//
// After a client calls "subscribe", the server pushes events as JSON-RPC
// notifications:
//
//	<- {"jsonrpc":"2.0","method":"event","params":{"type":"message.new","data":{...}}}
package rpc

import "encoding/json"

// Request is one inbound JSON-RPC call.
type Request struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one outbound JSON-RPC reply.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a server-initiated push (no ID). The only method the
// server emits is "event".
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  Event  `json:"params"`
}

// Event is the payload of an "event" notification.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Standard JSON-RPC codes plus gmcli application codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// CodeSendsDisabled: the daemon was started read-only (the default);
	// nothing may touch the phone.
	CodeSendsDisabled = 1001
	// CodeNotFound: the referenced entity does not exist.
	CodeNotFound = 1002
	// CodeAlreadyResolved: the approval was resolved by someone else first.
	CodeAlreadyResolved = 1003
	// CodeUnavailable: the phone or relay connection is not usable.
	CodeUnavailable = 1004
)

// Event types pushed to subscribed clients.
const (
	EventMessageNew          = "message.new"
	EventConversationUpdated = "conversation.updated"
	EventSyncStatus          = "sync.status"
	EventApprovalRequested   = "approval.requested"
	EventApprovalResolved    = "approval.resolved"
)

// SendMode controls how the daemon treats phone-mutating requests.
type SendMode string

const (
	// SendOff blocks all sends (daemon started with --read-only, the default).
	SendOff SendMode = "off"
	// SendApprove queues send.text requests as approvals; a human resolves
	// them via approvals.approve (TUI, `gmcli approvals approve`).
	SendApprove SendMode = "approve"
	// SendDirect performs send.text immediately. An audit row is still
	// written to the approvals table with a terminal status.
	SendDirect SendMode = "direct"
)
