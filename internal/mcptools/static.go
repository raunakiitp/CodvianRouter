package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// StaticServer is an in-process ToolServer: its tools and handlers
// live in this process. It exists for tests and for local stub
// servers — a ToolServer that needs no subprocess and no network.
// A nil handler for a listed tool answers an error, never a canned
// success.
type StaticServer struct {
	name     string
	tools    []mcp.Tool
	handlers map[string]func(ctx context.Context, args map[string]any) (string, error)
}

// NewStaticServer builds a StaticServer. tools is the tools/list
// answer (copied); handlers maps tool name to its implementation.
// A tool with no handler is listed but fails honestly on call.
func NewStaticServer(name string, tools []mcp.Tool, handlers map[string]func(ctx context.Context, args map[string]any) (string, error)) *StaticServer {
	cp := make([]mcp.Tool, len(tools))
	copy(cp, tools)
	if handlers == nil {
		handlers = map[string]func(ctx context.Context, args map[string]any) (string, error){}
	}
	return &StaticServer{name: name, tools: cp, handlers: handlers}
}

// Name returns the server name.
func (s *StaticServer) Name() string { return s.name }

// ListTools returns the configured tools.
func (s *StaticServer) ListTools(_ context.Context) ([]mcp.Tool, error) {
	out := make([]mcp.Tool, len(s.tools))
	copy(out, s.tools)
	return out, nil
}

// CallTool runs the handler for name, or errors when there is none.
func (s *StaticServer) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	h, ok := s.handlers[name]
	if !ok {
		return "", fmt.Errorf("tool %q is not implemented on server %q", name, s.name)
	}
	if args == nil {
		args = map[string]any{}
	}
	return h(ctx, args)
}

// ToolNames returns the tool names in sorted order (test helper).
func (s *StaticServer) ToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// StaticTool builds one mcp.Tool with an object input schema whose
// properties are the given JSON-schema fragments.
func StaticTool(name, description string, properties map[string]any, required ...string) mcp.Tool {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return mcp.Tool{Name: name, Description: description, InputSchema: schema}
}

// MarshalArgs is a test/convenience helper: it renders args as the
// JSON string a model tool call would carry.
func MarshalArgs(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
