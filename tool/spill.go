package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SpillStore keeps output that is too large for the context window somewhere
// it can be fetched from later, and says how. It is the store-and-locate seam
// behind Capture: the default keeps files on the local disk (LocalSpill), and a
// host can put an object store, a shared volume or anything addressable behind
// it instead (ToolContext.Spill, harness.QueryInput.SpillStore).
//
// norma never interprets what a store returns. The locator addresses bytes and
// nothing more; what a slice of a stored item means is the business of whoever
// reads it.
type SpillStore interface {
	// Store saves item and returns where it went. An error makes Capture fall
	// back to head+tail truncation, so a failing store costs the overflow,
	// never the call.
	Store(ctx context.Context, item SpillItem) (Spilled, error)
}

// SpillItem is one thing to keep.
type SpillItem struct {
	Data []byte
	// MediaType describes Data: MediaText for tool output, MediaJSON for an
	// encoded value (SpillValue).
	MediaType string
}

// Media types a SpillItem is stored with.
const (
	MediaText = "text/plain; charset=utf-8"
	MediaJSON = "application/json"
)

// Spilled is a stored item's receipt.
type Spilled struct {
	// Locator is the store's opaque address for the item (for LocalSpill, an
	// absolute file path). Hosts may keep it; norma passes it through.
	Locator string
	// Guidance is the sentence the model reads after the kept head: how to get
	// the rest. It should name the locator and a way to read from it that the
	// model has.
	Guidance string
}

// LocalSpill stores each item as a new file in Dir, which is created on
// demand. The locator is the file's absolute path: a relative one would only
// resolve from the directory it was computed against, and the reader is often
// elsewhere (a later command after a cd, a resumed session, a shell tool).
type LocalSpill struct{ Dir string }

// Store writes item to a new file and points at it.
func (l LocalSpill) Store(_ context.Context, item SpillItem) (Spilled, error) {
	ext := ".txt"
	if item.MediaType == MediaJSON {
		ext = ".json"
	}
	path, err := spillFile(l.Dir, item.Data, ext)
	if err != nil {
		return Spilled{}, err
	}
	return Spilled{Locator: path, Guidance: "Full output saved to  " + path}, nil
}

// spillStore is the store a call spills to: the host's, else a LocalSpill over
// OutputDir, else none (Capture truncates).
func spillStore(tc *ToolContext) SpillStore {
	switch {
	case tc == nil:
		return nil
	case tc.Spill != nil:
		return tc.Spill
	case tc.OutputDir != "":
		return LocalSpill{Dir: tc.OutputDir}
	}
	return nil
}

// SpillValue stores a value as JSON in the call's spill store, for a tool whose
// value is too large to keep in full but should stay retrievable. It returns
// false when the call has no store.
func SpillValue(ctx context.Context, tc *ToolContext, v any) (Spilled, bool, error) {
	store := spillStore(tc)
	if store == nil {
		return Spilled{}, false, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Spilled{}, true, err
	}
	sp, err := store.Store(ctx, SpillItem{Data: raw, MediaType: MediaJSON})
	return sp, true, err
}

func spillFile(dir string, data []byte, ext string) (string, error) {
	// Resolve the directory first: the returned pointer is what the model will
	// read, so it must not depend on anyone's current working directory.
	//
	// A failure here is reported rather than papered over with the relative
	// path. filepath.Abs only fails when dir is relative and os.Getwd fails —
	// i.e. the process's cwd has been deleted, which is precisely when a
	// relative pointer is most useless. Capture's caller falls back to
	// truncate(), which hands the model bounded output instead of a path it
	// cannot resolve.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("output-%d-%d%s", time.Now().Unix(), spillSeq.Add(1), ext)
	path := filepath.Join(abs, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
