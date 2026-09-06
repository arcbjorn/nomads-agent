// Package mcp implements a minimal Model Context Protocol server over stdio.
//
// The protocol is JSON-RPC 2.0 with a small method set, so it is implemented
// directly rather than pulling in a dependency.
package mcp

import "encoding/json"

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2024-11-05"

// request is an incoming JSON-RPC message.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether no reply is expected.
func (r request) isNotification() bool { return len(r.ID) == 0 }

// response is an outgoing JSON-RPC message.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// tool describes one callable tool.
type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`
}

// schema is a JSON Schema object describing a tool's arguments.
type schema struct {
	Type       string            `json:"type"`
	Properties map[string]schema `json:"properties,omitempty"`
	Items      *schema           `json:"items,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Enum       []string          `json:"enum,omitempty"`

	Description string `json:"description,omitempty"`
}

// content is one piece of a tool result.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the reply to tools/call.
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// obj is a convenience constructor for an object schema property.
func obj(desc string, props map[string]schema, required ...string) schema {
	return schema{Type: "object", Description: desc, Properties: props, Required: required}
}

// str builds a string property.
func str(desc string) schema { return schema{Type: "string", Description: desc} }

// boolean builds a boolean property.
func boolean(desc string) schema { return schema{Type: "boolean", Description: desc} }

// arrayOf builds an array property.
func arrayOf(item schema, desc string) schema {
	return schema{Type: "array", Description: desc, Items: &item}
}
