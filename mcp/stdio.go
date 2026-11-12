package mcp

import (
	"context"
	"io"
	"os/exec"

	"github.com/0xmanhnv/corvid/tool"
)

// stdioCloser shuts down a spawned MCP server process and its pipes.
type stdioCloser struct {
	cmd   *exec.Cmd
	stdin io.Closer
}

func (s *stdioCloser) Close() error {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return nil
}

// NewStdioClient spawns an MCP server as a subprocess, performs the handshake,
// and returns a connected Client (FR-10.1). Call Close to terminate it.
//
// env holds variables the server needs (e.g. the API token it uses), added on
// top of the environment the process-wide tool.ChildProcess settings give a
// child: the whole parent environment with none installed, the allowlist plus
// KeepEnv with Allowlist or CleanEnv. Pass nil for none.
func NewStdioClient(ctx context.Context, server, command string, env map[string]string, args ...string) (*Client, error) {
	cmd := exec.Command(command, args...)
	// The server starts from the child-environment allowlist plus env, never the
	// whole parent environment (see tool.ChildProcess); Deny and the credential
	// apply as to any child.
	var extra []string
	for k, v := range env {
		extra = append(extra, k+"="+v)
	}
	if err := tool.PrepareChild(cmd, extra, true); err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		server: server,
		conn:   newConn(stdout, stdin),
		closer: &stdioCloser{cmd: cmd, stdin: stdin},
	}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// newClientWithConn builds a Client over an arbitrary transport (used by tests).
func newClientWithConn(ctx context.Context, server string, r io.Reader, w io.Writer, closer io.Closer) (*Client, error) {
	c := &Client{server: server, conn: newConn(r, w), closer: closer}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
