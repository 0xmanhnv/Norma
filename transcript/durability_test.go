package transcript

import (
	"os"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// countingFile counts the writes and syncs that reach the transcript file.
type countingFile struct {
	*os.File
	writes, syncs *int
}

func (c countingFile) Write(p []byte) (int, error) { *c.writes++; return c.File.Write(p) }
func (c countingFile) Sync() error                 { *c.syncs++; return c.File.Sync() }

func countAppends(t *testing.T) (writes, syncs *int) {
	t.Helper()
	writes, syncs = new(int), new(int)
	orig := openAppend
	openAppend = func(path string) (appendFile, error) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		return countingFile{f, writes, syncs}, nil
	}
	t.Cleanup(func() { openAppend = orig })
	return writes, syncs
}

func textRecord(id, text string) Record {
	m := llm.UserText(text)
	return Record{UUID: id, SessionID: "s", Type: "message", Message: &m}
}

// A record and its newline reach the file in one write, whatever the record's
// size, so the file never holds a record without its line ending.
func TestAppendIsOneWrite(t *testing.T) {
	cases := []struct {
		name      string
		sizes     []int // text size of each record in the one Append call
		sync      bool
		wantSyncs int
	}{
		{"small record", []int{10}, false, 0},
		{"record over 4 KB", []int{5000}, false, 0},
		{"record over 64 KB", []int{70000}, false, 0},
		{"several records in one call", []int{10, 5000, 10}, false, 0},
		{"with sync on", []int{5000}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writes, syncs := countAppends(t)
			s := NewStore(t.TempDir())
			s.SetSync(c.sync)
			var recs []Record
			for i, n := range c.sizes {
				recs = append(recs, textRecord(string(rune('a'+i)), strings.Repeat("x", n)))
			}
			path := s.MainPath("s")
			if err := s.Append(path, recs...); err != nil {
				t.Fatal(err)
			}
			if *writes != 1 || *syncs != c.wantSyncs {
				t.Fatalf("append made %d write(s) and %d sync(s), want 1 and %d", *writes, *syncs, c.wantSyncs)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(raw), "\n") || strings.Count(string(raw), "\n") != len(recs) {
				t.Fatalf("file holds %d newline(s) for %d record(s)", strings.Count(string(raw), "\n"), len(recs))
			}
			got, skipped, err := s.LoadReport(path)
			if err != nil || len(skipped) != 0 || len(got) != len(recs) {
				t.Fatalf("loaded %d record(s), skipped %v, err %v", len(got), skipped, err)
			}
		})
	}
}

// Load reads what it can from a damaged file instead of failing all of it, and
// LoadReport says what was left out.
func TestLoadToleratesDamagedLines(t *testing.T) {
	line := func(id string) string {
		return `{"uuid":"` + id + `","session_id":"s","timestamp":"t","type":"message","message":{"role":"user","content":[{"type":"text","text":"` + id + `"}]}}`
	}
	torn := line("torn")[:60]
	cases := []struct {
		name      string
		file      string
		wantIDs   string // uuids loaded, joined
		wantLines []int  // line numbers skipped
		wantFinal bool   // the last skipped line is reported as an incomplete final line
	}{
		{"intact", line("a") + "\n" + line("b") + "\n", "a,b", nil, false},
		{"torn final line", line("a") + "\n" + line("b") + "\n" + torn, "a,b", []int{3}, true},
		{"torn only line", torn, "", []int{1}, true},
		{"torn line in the middle", line("a") + "\n" + torn + "\n" + line("b") + "\n", "a,b", []int{2}, false},
		{"garbage final line with a newline", line("a") + "\nnot json\n", "a", []int{2}, false},
		{"two records glued on one line", line("a") + line("b") + "\n" + line("c") + "\n", "a,b,c", nil, false},
		{"torn record glued to the next", line("a") + "\n" + torn + line("b") + "\n" + line("c") + "\n", "a,c", []int{2}, false},
		{"blank lines", line("a") + "\n\n  \n" + line("b") + "\n", "a,b", nil, false},
		{"final line complete but no newline", line("a") + "\n" + line("b"), "a,b", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			path := s.MainPath("s")
			if err := os.WriteFile(path, []byte(c.file), 0o644); err != nil {
				t.Fatal(err)
			}
			recs, skipped, err := s.LoadReport(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var ids []string
			for _, r := range recs {
				ids = append(ids, r.UUID)
			}
			if got := strings.Join(ids, ","); got != c.wantIDs {
				t.Fatalf("loaded %q, want %q", got, c.wantIDs)
			}
			if len(skipped) != len(c.wantLines) {
				t.Fatalf("skipped %v, want lines %v", skipped, c.wantLines)
			}
			for i, k := range skipped {
				if k.Line != c.wantLines[i] || k.Err == nil || k.Bytes == 0 {
					t.Fatalf("skipped[%d] = %+v, want line %d with an error", i, k, c.wantLines[i])
				}
			}
			if n := len(skipped); n > 0 && skipped[n-1].Final != c.wantFinal {
				t.Fatalf("Final = %v, want %v", skipped[n-1].Final, c.wantFinal)
			}
			plain, err := s.Load(path)
			if err != nil || len(plain) != len(recs) {
				t.Fatalf("Load = %d record(s), err %v; want %d and no error", len(plain), err, len(recs))
			}
		})
	}
}

// After a write that was cut short, the next append starts on a fresh line, so
// the torn record costs only itself.
func TestAppendAfterATornWrite(t *testing.T) {
	cases := []struct {
		name string
		tail string // what the file ends with before the append
		want int    // records loaded afterwards
	}{
		{"file ends in a newline", "", 2},
		{"file ends in a torn record", `{"uuid":"torn","type":"mess`, 2},
		{"file ends in a complete record with no newline", `{"uuid":"whole","session_id":"s","timestamp":"t","type":"rewind"}`, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			path := s.MainPath("s")
			if err := s.Append(path, textRecord("a", "one")); err != nil {
				t.Fatal(err)
			}
			if c.tail != "" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				f.WriteString(c.tail)
				f.Close()
			}
			if err := s.Append(path, textRecord("b", "two")); err != nil {
				t.Fatal(err)
			}
			recs, err := s.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != c.want || recs[0].UUID != "a" || recs[len(recs)-1].UUID != "b" {
				t.Fatalf("loaded %d record(s) %+v, want %d from a to b", len(recs), recs, c.want)
			}
		})
	}
}
