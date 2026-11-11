package harness

import "github.com/0xmanhnv/corvid/llm"

// RequestPrefixAware is optionally implemented by a Compactor that makes model
// calls of its own (a summary) and wants them to share the run's prompt cache.
// The loop calls SetRequestPrefix once when a run starts, with the request it
// is going to send minus the conversation: the system prompt and boundary, the
// tool list, the session settings, and in Messages whatever the loop puts in
// front of the conversation (the system reminder). A call built on that prefix
// and followed by the conversation reads the cache the run's own requests
// wrote, instead of paying for the whole history as fresh input.
type RequestPrefixAware interface {
	SetRequestPrefix(prefix llm.CompletionRequest)
}

// CompactionUsageReporter is optionally implemented by a Compactor whose work
// costs tokens. The loop calls TakeUsage after each Pre and Reactive and adds
// the result to the run's usage, so a summary is accounted for like any other
// model call. TakeUsage returns the usage since it was last called.
type CompactionUsageReporter interface {
	TakeUsage() llm.Usage
}

// announcePrefix hands the run's request prefix to a Compactor that asked for
// it. schemas is the tool list the run sends.
func (l *loop) announcePrefix(schemas []llm.ToolSchema) {
	pa, ok := l.in.Compactor.(RequestPrefixAware)
	if !ok {
		return
	}
	prefix := llm.CompletionRequest{
		System:          l.in.System,
		DynamicBoundary: l.in.DynamicBoundary,
		Tools:           schemas,
		Temperature:     l.in.Temperature,
	}
	l.in.ModelSettings.Apply(&prefix)
	if l.in.SystemReminder != "" {
		prefix.Messages = []llm.Message{llm.UserText(l.in.SystemReminder)}
	}
	pa.SetRequestPrefix(prefix)
}

// absorbCompactionUsage adds what the Compactor spent since the last call to
// the run's usage.
func (l *loop) absorbCompactionUsage() {
	if r, ok := l.in.Compactor.(CompactionUsageReporter); ok {
		l.usage.Add(r.TakeUsage())
	}
}
