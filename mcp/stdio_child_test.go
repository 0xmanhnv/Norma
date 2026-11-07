package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0xmanhnv/norma/tool"
)

// A stdio MCP server is a child like any other: Deny refuses it, CleanEnv
// scrubs the server's secrets but keeps the server's own configured env.
func TestStdioChildProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	deny := errors.New("jail not active")
	tool.SetChildProcess(&tool.ChildProcess{Deny: deny})
	t.Cleanup(func() { tool.SetChildProcess(nil) })
	if _, err := NewStdioClient(context.Background(), "x", "sh", nil, "-c", "true"); !errors.Is(err, deny) {
		t.Fatalf("spawn must be refused, got %v", err)
	}

	t.Setenv("STDIO_TEST_SECRET", "s3cret")
	out := filepath.Join(t.TempDir(), "env")
	tool.SetChildProcess(&tool.ChildProcess{CleanEnv: true, Home: "/home/tool"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The "server" dumps its env and exits, so the handshake fails; the env is what we check.
	_, _ = NewStdioClient(ctx, "x", "sh", map[string]string{"MCP_OWN": "1"}, "-c", "env > "+out)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	if strings.Contains(env, "STDIO_TEST_SECRET") || !strings.Contains(env, "MCP_OWN=1") || !strings.Contains(env, "HOME=/home/tool") {
		t.Fatalf("stdio env:\n%s", env)
	}
}
