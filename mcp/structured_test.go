package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
)

// structuredServer answers tools/call with text content plus a structured
// result. The "typed" tool declares an outputSchema; "free" does not.
func structuredServer(in io.Reader, out io.Writer, structured any) {
	sc := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"temp": map[string]any{"type": "number"}},
		"required":   []any{"temp"},
	}
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		id, hasID := req["id"]
		if !hasID {
			continue
		}
		var result any
		switch req["method"] {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "typed", "inputSchema": map[string]any{"type": "object"}, "outputSchema": schema},
				map[string]any{"name": "free", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			result = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "22.5 degrees"}},
				"structuredContent": structured,
			}
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

func callStructured(t *testing.T, structured any, name string) (string, any) {
	t.Helper()
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	go structuredServer(sReader, sWriter, structured)
	ctx := context.Background()
	client, err := newClientWithConn(ctx, "srv", cReader, cWriter, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Name() == ToolName("srv", name) {
			res, err := tl.Call(ctx, []byte(`{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			return res.Flatten(), res.Data
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

// The structured result is kept as the call's value instead of being dropped;
// the text the model reads is unchanged.
func TestStructuredContentIsKept(t *testing.T) {
	for _, name := range []string{"typed", "free"} {
		text, data := callStructured(t, map[string]any{"temp": 22.5}, name)
		if text != "22.5 degrees" {
			t.Fatalf("%s: text = %q", name, text)
		}
		v, ok := data.(StructuredContent)
		if !ok || string(v) != `{"temp":22.5}` {
			t.Fatalf("%s: Data = %#v", name, data)
		}
	}
}

// Trap: a value that breaks the declared outputSchema is not passed on as if it
// had the declared shape. Without a declaration nothing is checked.
func TestStructuredContentMustMatchOutputSchema(t *testing.T) {
	text, data := callStructured(t, map[string]any{"temp": "warm"}, "typed")
	if text != "22.5 degrees" || data != nil {
		t.Fatalf("typed: text=%q data=%#v", text, data)
	}
	if _, data := callStructured(t, map[string]any{"temp": "warm"}, "free"); data == nil {
		t.Fatal("free: an undeclared shape must be kept")
	}
	if _, data := callStructured(t, nil, "free"); data != nil {
		t.Fatalf("null structured content: Data = %#v", data)
	}
}
