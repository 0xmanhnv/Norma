package transcript

import (
	"errors"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

func compactionSet() (llm.BoundaryMeta, []llm.Message) {
	meta := llm.BoundaryMeta{Trigger: "auto", PreTokens: 9000, MessagesSummarized: 4}
	return meta, []llm.Message{
		llm.BoundaryMessage(meta),
		llm.UserText("summary of what came before"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("kept tail 1")}},
		llm.UserText("kept tail 2"),
	}
}

// The set a compaction leaves behind — boundary record, boundary marker,
// summary, kept tail — reaches the file in ONE write. Record by record, a
// crash in the middle left a boundary with part of its tail, and a resume hid
// the rest of the history behind that boundary.
func TestRecordCompactionIsOneWrite(t *testing.T) {
	writes, _ := countAppends(t)
	s := NewStore(t.TempDir())
	w := s.NewWriter("sess", "")
	w.RecordMessage(llm.UserText("before"), llm.Usage{})
	*writes = 0

	meta, msgs := compactionSet()
	w.RecordCompaction(meta, msgs)
	if *writes != 1 {
		t.Fatalf("%d writes for one compaction, want 1", *writes)
	}
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Load(s.MainPath("sess"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2+len(msgs) {
		t.Fatalf("%d records, want %d", len(recs), 2+len(msgs))
	}
	// Same records, in the same order, as RecordBoundary + RecordMessage each
	// would have written, and chained like any others.
	if recs[1].Type != "boundary" || recs[1].Boundary == nil || recs[1].Boundary.PreTokens != 9000 {
		t.Fatalf("boundary record: %+v", recs[1])
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].ParentUUID != recs[i-1].UUID || recs[i].UUID == "" || recs[i].SessionID != "sess" {
			t.Fatalf("record %d is not chained to the one before it", i)
		}
	}
	got, _ := Messages(recs)
	view := llm.MessagesForAPI(got)
	if len(view) != 3 || view[0].Text() != "summary of what came before" || view[2].Text() != "kept tail 2" {
		t.Fatalf("resumed view is not summary + whole tail: %+v", view)
	}
	// A message recorded afterwards chains to the last record of the set.
	w.RecordMessage(llm.UserText("after"), llm.Usage{})
	recs, _ = s.Load(s.MainPath("sess"))
	if n := len(recs); recs[n-1].ParentUUID != recs[n-2].UUID {
		t.Fatal("the writer lost its place after a compaction")
	}
}

// failingFile rejects every write.
type failingFile struct{ appendFile }

func (failingFile) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A compaction that cannot be written leaves nothing behind: no boundary
// without its tail, and the next record still chains to the last good one.
func TestRecordCompactionAllOrNothing(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("sess", "")
	w.RecordMessage(llm.UserText("before"), llm.Usage{})

	orig := openAppend
	openAppend = func(path string) (appendFile, error) {
		f, err := orig(path)
		if err != nil {
			return nil, err
		}
		return failingFile{f}, nil
	}
	meta, msgs := compactionSet()
	w.RecordCompaction(meta, msgs)
	openAppend = orig

	if w.Err() == nil {
		t.Fatal("the failed write was not reported")
	}
	recs, _ := s.Load(s.MainPath("sess"))
	if len(recs) != 1 {
		t.Fatalf("%d records after a failed compaction write, want the 1 from before", len(recs))
	}
	w.RecordMessage(llm.UserText("after"), llm.Usage{})
	recs, _ = s.Load(s.MainPath("sess"))
	if len(recs) != 2 || recs[1].ParentUUID != recs[0].UUID {
		t.Fatalf("the writer did not keep its place: %+v", recs)
	}
	if view := llm.MessagesForAPI(mustMessages(recs)); len(view) != 2 {
		t.Fatalf("view after a failed compaction write: %d messages, want the full history (2)", len(view))
	}
}

func mustMessages(recs []Record) []llm.Message {
	m, _ := Messages(recs)
	return m
}
