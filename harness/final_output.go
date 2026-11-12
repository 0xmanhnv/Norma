package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xmanhnv/corvid/tool"
)

// DefaultOutputRepairs is the number of repair turns a run with a FinalOutput
// gets when MaxRepairs is 0.
const DefaultOutputRepairs = 1

// FinalOutput asks for the run's final answer as a JSON document that satisfies
// a schema. When the model ends its turn, its text is checked; an answer that
// is not JSON or breaks the schema is sent back as a user turn naming where and
// which rule (never the content of the answer), up to MaxRepairs times, and the
// run then ends with ReasonInvalidOutput. A valid answer ends the run as usual,
// with ReasonCompleted and the document in Terminal.Output.
//
// The loop does not show the schema to the model: the host says what to answer
// in its prompt. Only a run that ends with ReasonCompleted is checked; a run
// that ends any other way (a budget, a refusal, an error) ends as before.
type FinalOutput struct {
	// Schema is the JSON Schema of the answer, checked with the validator of
	// tool inputs (tool.ValidateInput): type, required, properties, items and
	// enum. Other keywords are not checked. Read-only: sessions may share it.
	Schema map[string]any
	// MaxRepairs is how many repair turns a run may take: 0 means
	// DefaultOutputRepairs, a negative value means none (the first invalid
	// answer ends the run).
	MaxRepairs int
}

func (f *FinalOutput) repairs() int {
	switch {
	case f.MaxRepairs < 0:
		return 0
	case f.MaxRepairs == 0:
		return DefaultOutputRepairs
	}
	return f.MaxRepairs
}

// OutputError is the cause of Terminal.Err when a run ends with
// ReasonInvalidOutput: the final answer still did not satisfy
// QueryInput.FinalOutput after the repairs allowed. Terminal.Err carries
// tool.CodeInvalidOutput; errors.As finds the OutputError behind it.
type OutputError struct {
	// Problem is what was wrong with the last answer: a location and the rule
	// it broke, without any of the answer's content.
	Problem string
	// Repairs is the number of repair turns the run took.
	Repairs int
}

func (e *OutputError) Error() string {
	return fmt.Sprintf("final output does not match the schema after %d repair(s): %s", e.Repairs, e.Problem)
}

// repairPromptFormat is the user turn that asks the model to fix its answer.
// It names the problem and never quotes the answer back.
const repairPromptFormat = "Your final answer must be one JSON document that matches the required schema, and it does not: %s. " +
	"Reply with the corrected JSON document only, with no other text."

// checkFinalOutput checks a final answer against schema. It returns the JSON
// document when the answer is valid, or a problem that says where it is wrong
// without repeating any of the answer.
//
// The answer is the whole text, trimmed; a single Markdown code fence around
// it (```json … ```) is accepted and removed.
func checkFinalOutput(schema map[string]any, text string) (json.RawMessage, string) {
	doc := unfence(strings.TrimSpace(text))
	if doc == "" {
		return nil, "the answer is empty"
	}
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return nil, fmt.Sprintf("the answer is not valid JSON (syntax error at byte %d)", syn.Offset)
		}
		return nil, "the answer is not valid JSON"
	}
	if err := tool.ValidateInput(schema, json.RawMessage(doc)); err != nil {
		var se *tool.SchemaError
		if !errors.As(err, &se) {
			return nil, "the answer does not match the schema"
		}
		where := se.Path
		if where == "" {
			where = "the document"
		}
		switch se.Keyword {
		case "required":
			return nil, fmt.Sprintf("%s: missing required property %q", where, se.Want)
		case "enum":
			return nil, fmt.Sprintf("%s: must be one of %s", where, se.Want)
		default:
			return nil, fmt.Sprintf("%s: must be of type %s", where, se.Want)
		}
	}
	return json.RawMessage(doc), ""
}

// unfence removes one Markdown code fence around s, with or without a language
// tag. Text that is not wholly fenced is returned as it is.
func unfence(s string) string {
	if len(s) < 6 || !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") {
		return s
	}
	body := s[3 : len(s)-3]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		if tag := strings.TrimSpace(body[:i]); !strings.ContainsAny(tag, "{[\"") {
			body = body[i+1:]
		}
	}
	if strings.Contains(body, "```") {
		return s // more than one fence: not a single fenced document
	}
	return strings.TrimSpace(body)
}
