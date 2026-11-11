// Package transcript persists a conversation to an append-only JSONL log on
// disk and reconstructs it for resume. The main session is written to
// <dir>/<sessionID>.jsonl; each
// subagent is written to its own sidechain file under
// <dir>/<sessionID>/subagents/agent-<agentID>.jsonl, so subagent tokens are
// counted and stored separately (aggregate them offline if needed).
//
// The log is the full-fidelity history: every user/assistant/tool message is
// recorded as it is born. When compaction inserts a boundary, the post-boundary
// working set — the boundary marker, the summary, and the kept recent tail — is
// ALSO persisted as messages, so a cold-start resume restores the COMPACTED view
// (via llm.MessagesForAPI, which slices from the last boundary) instead of
// replaying raw history and re-summarizing. The kept tail
// is re-recorded after the boundary (it also exists earlier as births); only the
// post-boundary copy reaches the model. Lightweight working-set edits that do not
// add a boundary (MicroCompact tool-result clearing) stay ephemeral and
// are not persisted. A separate Type=="boundary" record is written for
// observability and is skipped during reconstruction.
//
// Each Append reaches the file as one write, record and newline together. A
// write cut short by a crash leaves at most an incomplete final line, which
// Load skips (LoadReport says so) and the next Append closes off with a
// newline, so one torn record never costs the rest of the session.
// Store.SetSync adds an fsync per append for hosts that need the transcript to
// survive power loss.
//
// A Type=="rewind" record moves the conversation back to an earlier message
// (Store.Rewind): reconstruction drops every message after RewindTo. Nothing
// is deleted from the file, so the abandoned turns stay readable, and the next
// resume continues from the rewound point.
package transcript

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0xmanhnv/corvid/llm"
)

// Record is one line of a JSONL transcript.
type Record struct {
	UUID        string            `json:"uuid"`
	ParentUUID  string            `json:"parent_uuid,omitempty"`
	SessionID   string            `json:"session_id"`
	AgentID     string            `json:"agent_id,omitempty"`
	IsSidechain bool              `json:"is_sidechain,omitempty"`
	Timestamp   string            `json:"timestamp"`
	Type        string            `json:"type"` // "message" | "boundary" | "rewind"
	Message     *llm.Message      `json:"message,omitempty"`
	Usage       *llm.Usage        `json:"usage,omitempty"`    // assistant turns only
	Boundary    *llm.BoundaryMeta `json:"boundary,omitempty"` // Type=="boundary"
	// RewindTo is the UUID of the last message kept (Type=="rewind"); "" keeps
	// none.
	RewindTo string `json:"rewind_to,omitempty"`
}

// Store is a directory of session transcripts.
type Store struct {
	dir  string
	mu   sync.Mutex // serializes appends across writers in this process
	sync bool       // fsync after every append (SetSync)
}

// NewStore returns a Store rooted at dir (created lazily on first write).
func NewStore(dir string) *Store { return &Store{dir: dir} }

// MainPath is the JSONL file for a top-level session.
func (s *Store) MainPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".jsonl")
}

// AgentPath is the sidechain JSONL file for a subagent of a session.
func (s *Store) AgentPath(sessionID, agentID string) string {
	return filepath.Join(s.dir, sessionID, "subagents", "agent-"+agentID+".jsonl")
}

// SetSync makes every Append flush the file to stable storage (fsync) before
// it returns. Off by default: an append then survives the process being killed
// but not necessarily the machine losing power. Turn it on when a transcript
// must survive that too; each append then costs one fsync. Set it before the
// store is used.
func (s *Store) SetSync(on bool) { s.sync = on }

// appendFile is what Append needs from the transcript file. It is a seam for
// tests; production code always gets an *os.File.
type appendFile interface {
	io.Writer
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

var openAppend = func(path string) (appendFile, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
}

// Append writes records to path as JSONL, creating parent directories. It is
// safe for concurrent use within the process.
//
// All the records and their newlines go to the file in a single write, so a
// record is never on disk without its line ending because the process died
// between two writes. If the file does not end in a newline (an earlier write
// was cut short), a newline is written first, so the new records start on a
// line of their own instead of being glued to the torn one.
func (s *Store) Append(path string, recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := openAppend(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			buf.WriteByte('\n')
		}
	}
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			f.Close()
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if s.sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// Skipped describes one line of a transcript file that could not be read and
// was left out of the loaded records.
type Skipped struct {
	Line  int  // 1-based line number in the file
	Bytes int  // length of the line
	Final bool // the last line of the file, with no newline after it: a write that was cut short
	Err   error
}

func (k Skipped) String() string {
	what := "corrupt line"
	if k.Final {
		what = "incomplete final line"
	}
	return fmt.Sprintf("transcript: skipped %s %d (%d bytes): %v", what, k.Line, k.Bytes, k.Err)
}

// Load reads all records from a transcript file. A missing file yields no
// records and no error (a fresh session). A line that cannot be decoded — in
// practice the final line of a file whose last write was cut short — is
// skipped rather than failing the whole file; use LoadReport to learn which
// lines were skipped.
func (s *Store) Load(path string) ([]Record, error) {
	recs, _, err := s.LoadReport(path)
	return recs, err
}

// LoadReport is Load that also returns the lines it skipped, in file order, so
// a caller can warn about them or refuse a damaged transcript. A line holding
// several complete records back to back (written before Append kept every
// record on its own line) is read in full and not reported.
func (s *Store) LoadReport(path string) ([]Record, []Skipped, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	var recs []Record
	var skipped []Skipped
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tolerate large tool results
	var read int64
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		read += int64(len(line)) + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		got, err := decodeLine(line)
		if err != nil {
			skipped = append(skipped, Skipped{Line: n, Bytes: len(line), Err: err})
			continue
		}
		recs = append(recs, got...)
	}
	if err := sc.Err(); err != nil {
		return recs, skipped, err
	}
	// The scanner strips line endings, so a last line with none shows up as
	// the bytes read running one past the file size.
	if len(skipped) > 0 && skipped[len(skipped)-1].Line == n {
		if st, err := f.Stat(); err == nil && read > st.Size() {
			skipped[len(skipped)-1].Final = true
		}
	}
	return recs, skipped, nil
}

// decodeLine decodes the record on a line, or the records when several sit on
// it back to back. Any undecodable part fails the whole line.
func decodeLine(line []byte) ([]Record, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	var out []Record
	for {
		var r Record
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) && len(out) > 0 {
				return out, nil
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		out = append(out, r)
	}
}

// Messages reconstructs the raw conversation and cumulative usage from records.
// Boundary records are skipped — the working set is rebuilt by compaction on the
// next turn, not replayed from disk. Rewind records drop the messages after
// their target; usage still counts every turn, since those tokens were spent.
func Messages(recs []Record) ([]llm.Message, llm.Usage) {
	live := Live(recs)
	msgs := make([]llm.Message, 0, len(live))
	for _, r := range live {
		msgs = append(msgs, *r.Message)
	}
	var total llm.Usage
	for _, r := range recs {
		if r.Type != "boundary" && r.Message != nil && r.Usage != nil {
			total.Add(*r.Usage)
		}
	}
	return msgs, total
}

// Live returns the message records that make up the conversation after every
// rewind is applied, in order. A host uses their UUIDs to offer rewind points.
func Live(recs []Record) []Record {
	var live []Record
	for _, r := range recs {
		switch {
		case r.Type == "rewind":
			live = live[:rewindIndex(live, r.RewindTo)]
		case r.Type == "boundary" || r.Message == nil:
		default:
			live = append(live, r)
		}
	}
	return live
}

// rewindIndex is the number of live records a rewind to uuid keeps. A target
// that is not live (written by hand, or already rewound past) keeps them all.
func rewindIndex(live []Record, uuid string) int {
	if uuid == "" {
		return 0
	}
	for i := len(live) - 1; i >= 0; i-- {
		if live[i].UUID == uuid {
			return i + 1
		}
	}
	return len(live)
}

// ErrNotLive is returned by Rewind for a target that is not a live message of
// the session.
// It carries llm.CodeInvalidArgument; compare with errors.Is.
var ErrNotLive error = llm.NewError(llm.CodeInvalidArgument, "transcript: rewind target is not a live message of the session")

// Rewind moves a main session back so that toUUID is its last message ("" =
// empty). It appends a rewind record and deletes nothing. Call it only while
// no Writer is appending to the session: a running writer would chain its next
// record after the old tail.
func (s *Store) Rewind(sessionID, toUUID string) error {
	path := s.MainPath(sessionID)
	recs, err := s.Load(path)
	if err != nil {
		return err
	}
	if toUUID != "" {
		live := Live(recs)
		if rewindIndex(live, toUUID) == len(live) && (len(live) == 0 || live[len(live)-1].UUID != toUUID) {
			return ErrNotLive
		}
	}
	return s.Append(path, Record{
		UUID:       newID(),
		ParentUUID: LastUUID(recs),
		SessionID:  sessionID,
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Type:       "rewind",
		RewindTo:   toUUID,
	})
}

// LastUUID returns the UUID of the last record, for continuing the parent chain.
func LastUUID(recs []Record) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[len(recs)-1].UUID
}

// Writer binds a Store, a file path, and a session/agent identity, assigning a
// UUID and parent link to each record. It satisfies harness.Recorder. Append
// errors are retained (see Err) rather than panicking, so a failing disk
// degrades to in-memory operation instead of crashing the agent loop.
type Writer struct {
	store       *Store
	path        string
	sessionID   string
	agentID     string
	isSidechain bool

	mu   sync.Mutex
	last string
	now  func() time.Time
	err  error
}

// NewWriter returns a Writer for a fresh transcript file. agentID == "" writes
// the main session file; a non-empty agentID writes a subagent sidechain.
func (s *Store) NewWriter(sessionID, agentID string) *Writer {
	w := &Writer{store: s, sessionID: sessionID, agentID: agentID, now: time.Now}
	if agentID == "" {
		w.path = s.MainPath(sessionID)
	} else {
		w.path = s.AgentPath(sessionID, agentID)
		w.isSidechain = true
	}
	return w
}

// ResumeWriter returns a Writer that continues an existing main-session file,
// chaining new records after lastUUID.
func (s *Store) ResumeWriter(sessionID, lastUUID string) *Writer {
	w := s.NewWriter(sessionID, "")
	w.last = lastUUID
	return w
}

// Err returns the first append error encountered, if any.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) write(r Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r.SessionID = w.sessionID
	r.AgentID = w.agentID
	r.IsSidechain = w.isSidechain
	r.UUID = newID()
	r.ParentUUID = w.last
	r.Timestamp = w.now().UTC().Format(time.RFC3339Nano)
	if err := w.store.Append(w.path, r); err != nil {
		if w.err == nil {
			w.err = err
		}
		return
	}
	w.last = r.UUID
}

// RecordMessage persists a conversation message. usage is attributed to this
// turn (zero for non-assistant messages).
func (w *Writer) RecordMessage(m llm.Message, usage llm.Usage) {
	rec := Record{Type: "message", Message: &m}
	if usage != (llm.Usage{}) {
		u := usage
		rec.Usage = &u
	}
	w.write(rec)
}

// RecordBoundary persists a compaction boundary for observability.
func (w *Writer) RecordBoundary(meta llm.BoundaryMeta) {
	m := meta
	w.write(Record{Type: "boundary", Boundary: &m})
}

// RecordCompaction persists what a compaction leaves after its boundary — the
// boundary record, then msgs (the boundary marker, the summary and the kept
// tail) — as one append, which is one write to the file.
//
// Written record by record, a crash part-way left the boundary and the
// summary on disk with only some of the tail behind them. On resume everything
// before the boundary is hidden, so the missing tail messages were gone from
// the model's view although they were in the file. In one write the set is
// there whole or (the process dying inside the write aside) not at all, and a
// resume without it falls back to the full history before the boundary.
//
// It implements harness.CompactionRecorder.
func (w *Writer) RecordCompaction(meta llm.BoundaryMeta, msgs []llm.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	recs := make([]Record, 0, len(msgs)+1)
	last := w.last
	add := func(r Record) {
		r.SessionID = w.sessionID
		r.AgentID = w.agentID
		r.IsSidechain = w.isSidechain
		r.UUID = newID()
		r.ParentUUID = last
		r.Timestamp = w.now().UTC().Format(time.RFC3339Nano)
		last = r.UUID
		recs = append(recs, r)
	}
	m := meta
	add(Record{Type: "boundary", Boundary: &m})
	for i := range msgs {
		add(Record{Type: "message", Message: &msgs[i]})
	}
	if err := w.store.Append(w.path, recs...); err != nil {
		if w.err == nil {
			w.err = err
		}
		return
	}
	w.last = last
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand should never fail; fall back to a time-based id to stay unique-ish.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// NewSessionID returns a fresh session identifier.
func NewSessionID() string { return newID() }

// NewAgentID returns a fresh subagent identifier.
func NewAgentID() string { return newID() }
