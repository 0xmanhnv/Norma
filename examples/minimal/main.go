// Example minimal is the smallest useful Norma agent: a provider, a
// host-owned system prompt, the default file/shell toolset, and the streaming
// loop. It is the quickstart from the README, as a runnable program.
//
// Run it with a key for your chosen provider:
//
//	OPENAI_API_KEY=... AGENT_MODEL=gpt-4o go run ./examples/minimal
//
// Switch llm.Config.Format to llm.FormatAnthropic (and set ANTHROPIC_API_KEY)
// to drive an Anthropic-wire-format model with the same code.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/0xmanhnv/norma/agentcore"
	"github.com/0xmanhnv/norma/harness"
	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/permission"
)

func main() {
	// Point at any OpenAI- or Anthropic-wire-format model. With BaseURL/APIKey
	// unset, the provider falls back to the standard env vars and public endpoint.
	prov, err := llm.NewProvider(llm.Config{
		Format: llm.FormatOpenAI,
		Model:  envOr("AGENT_MODEL", "gpt-4o"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// The SDK injects no system prompt of its own — this one is entirely yours.
	// The default toolset (Read/Write/Edit/LS/Glob/Grep/Bash/...) is used because
	// Options.Tools is left unset.
	sess := agentcore.NewSession(agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"You are a concise assistant. Use tools when they help, and stop when the task is done."},
		PermissionMode: permission.ModeBypass, // auto-approve — for a local demo only
	})

	const prompt = "List the Go files in the current directory and say in one line what each is for."
	for ev, err := range sess.Prompt(context.Background(), prompt) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		switch ev.Kind {
		case harness.KindText:
			fmt.Print(ev.Text)
		case harness.KindToolUse:
			fmt.Printf("\n[tool: %s]\n", ev.ToolUse.Name)
		}
	}
	fmt.Println()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
