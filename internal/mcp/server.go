package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/arcbjorn/nomads-agent/internal/client"
	nsync "github.com/arcbjorn/nomads-agent/internal/sync"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// Server exposes the Nomads integration to AI agents over stdio.
//
// Tools are high-level and speak desired state: an agent asks for the itinerary
// it wants and the server works out the minimal set of changes.
type Server struct {
	newClient func() (*client.Hybrid, error)

	// clientMu guards lazy client construction; outMu guards the writer. They
	// are separate because a tool call holds neither while calling the other.
	clientMu sync.Mutex
	client   *client.Hybrid

	outMu sync.Mutex
	out   *json.Encoder
}

// NewServer builds a server that lazily constructs its client, so a missing
// credential surfaces as a tool error rather than preventing startup.
func NewServer(newClient func() (*client.Hybrid, error)) *Server {
	return &Server{newClient: newClient}
}

// clientOrErr returns the shared client, building it on first use.
func (s *Server) clientOrErr() (*client.Hybrid, error) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	c, err := s.newClient()
	if err != nil {
		return nil, err
	}
	s.client = c
	return c, nil
}

// Serve reads JSON-RPC messages until the input closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	scanner := bufio.NewScanner(in)
	// MCP messages can be large; allow up to 8MB per line.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.send(response{JSONRPC: "2.0", Error: &rpcError{
				Code: codeParseError, Message: "invalid JSON"}})
			continue
		}
		s.handle(ctx, req)

		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handle dispatches one request.
func (s *Server) handle(ctx context.Context, req request) {
	switch req.Method {
	case "initialize":
		s.reply(req, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name": "nomads-agent", "version": "0.1.0",
			},
			"instructions": "Read and update a Nomads.com profile and travel itinerary. " +
				"Prefer nomads_plan_trip_sync to preview changes, then nomads_sync_trips to apply them. " +
				"Deletions never happen unless delete_missing is true.",
		})
	case "notifications/initialized", "initialized":
		// No reply expected.
	case "ping":
		s.reply(req, map[string]any{})
	case "tools/list":
		s.reply(req, map[string]any{"tools": tools()})
	case "tools/call":
		s.callTool(ctx, req)
	default:
		if req.isNotification() {
			return
		}
		s.send(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code: codeMethodNotFound, Message: "unknown method " + req.Method}})
	}
}

// callParams is the argument envelope for tools/call.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// callTool runs one tool and formats its result.
func (s *Server) callTool(ctx context.Context, req request) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.send(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code: codeInvalidParams, Message: "invalid params"}})
		return
	}

	result, err := s.dispatch(ctx, p.Name, p.Arguments)
	if err != nil {
		// Tool failures are returned as results with isError, so the agent can
		// read the semantic code and recover rather than seeing a transport error.
		s.reply(req, toolResult{
			Content: []content{{Type: "text", Text: errorJSON(err)}},
			IsError: true,
		})
		return
	}
	s.reply(req, toolResult{Content: []content{{Type: "text", Text: result}}})
}

// errorJSON renders a semantic error for an agent, without credentials.
func errorJSON(err error) string {
	code := nomads.CodeOf(err)
	if code == "" {
		code = nomads.ErrNetwork
	}
	payload := map[string]any{"error": string(code), "message": err.Error()}
	var ne *nomads.Error
	if ok := asNomads(err, &ne); ok && len(ne.Details) > 0 {
		payload["details"] = ne.Details
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return string(b)
}

// reply sends a successful response.
func (s *Server) reply(req request, result any) {
	if req.isNotification() {
		return
	}
	s.send(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// send writes one message.
func (s *Server) send(r response) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.out != nil {
		_ = s.out.Encode(r)
	}
}

// jsonResult renders a value as pretty JSON for a tool result.
func jsonResult(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(b), nil
}

// planSummary is the agent-facing shape of a sync plan.
type planSummary struct {
	Creates   []string               `json:"creates"`
	Updates   []string               `json:"updates"`
	Deletes   []string               `json:"deletes"`
	Unchanged []string               `json:"unchanged"`
	Ambiguous []ambiguousSummary     `json:"ambiguous"`
	Warnings  []string               `json:"warnings,omitempty"`
	Mutations int                    `json:"mutation_count"`
	Text      string                 `json:"human_readable"`
	Plan      map[string]any         `json:"-"`
	Detail    []nsync.AmbiguousMatch `json:"-"`
}

// summarise turns a plan into the agent-facing summary.
func summarise(p nsync.SyncPlan) planSummary {
	s := planSummary{
		Creates:   []string{},
		Updates:   []string{},
		Deletes:   []string{},
		Unchanged: []string{},
		Ambiguous: []ambiguousSummary{},
		Warnings:  p.Warnings,
		Mutations: p.MutationCount(),
		Text:      nsync.Format(p),
	}
	for _, c := range p.Creates {
		s.Creates = append(s.Creates, c.Label())
	}
	for _, u := range p.Updates {
		s.Updates = append(s.Updates, fmt.Sprintf("%s => %s (%s)",
			u.Current.Label(), u.Desired.Label(), strings.Join(u.Changes, ", ")))
	}
	for _, d := range p.Deletes {
		s.Deletes = append(s.Deletes, d.Label())
	}
	for _, u := range p.Unchanged {
		s.Unchanged = append(s.Unchanged, u.Current.Label())
	}
	for _, a := range p.Ambiguous {
		cands := make([]string, 0, len(a.Candidates))
		for _, c := range a.Candidates {
			cands = append(cands, c.Label())
		}
		s.Ambiguous = append(s.Ambiguous, ambiguousSummary{
			Desired: a.Desired.Label(), Candidates: cands, Reason: a.Reason,
		})
	}
	return s
}

// ambiguousSummary is an unresolved match, for the agent to arbitrate.
type ambiguousSummary struct {
	Desired    string   `json:"desired"`
	Candidates []string `json:"candidates"`
	Reason     string   `json:"reason"`
}
