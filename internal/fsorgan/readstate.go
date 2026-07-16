package fsorgan

import (
	"sync"
)

// readRegistry is the owned read-state that makes the read/write/edit family
// self-consistent: read records a full read of a path, and write/edit require
// that an EXISTING file was fully read — and not modified since — before it may
// be overwritten or edited. A new file (one that does not yet exist) needs no
// prior read.
//
// State is keyed by absolute path and is process-global (corpos is a single-user
// agent OS). The modified-since-read check is the backstop against a path read
// earlier being written after it changed on disk.
type readRegistry struct {
	mu sync.Mutex
	m  map[string]readMark
}

type readMark struct {
	mtimeMs int64 // file mtime (ms) observed at read time
	full    bool  // whole-file read (a ranged read is a partial view)
}

// newReadRegistry returns an empty registry.
func newReadRegistry() *readRegistry {
	return &readRegistry{m: make(map[string]readMark)}
}

// markRead records that path was observed at file mtime mtimeMs. full is false
// for a ranged/partial read (which does not satisfy the write/edit precondition).
//
// A partial read must NOT downgrade a still-valid full-read mark: a small-window
// worker pages a fully-read file in ranges (offset/limit) to fit its context, and
// each page must not revoke the edit permission its earlier whole-file read earned
// (the run-10 trap — the worker read the file, paged it, then fs.edit failed "not
// read yet"). So a non-full read is dropped when a full mark for the same path is
// still current (the file has not changed since it); if the file HAS changed, the
// full mark is stale and the partial read replaces it (correctly re-blocking until
// a fresh whole-file read).
func (r *readRegistry) markRead(path string, mtimeMs int64, full bool) {
	if r == nil {
		return
	}
	key := absPath(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !full {
		if prev, ok := r.m[key]; ok && prev.full && mtimeMs <= prev.mtimeMs {
			return // keep the still-valid full mark; a page must not revoke editing
		}
	}
	r.m[key] = readMark{mtimeMs: mtimeMs, full: full}
}

// checkWritable reports whether an existing path may be overwritten/edited given
// its current mtime, returning a caller-facing reason when not. The reason is
// actionable: it distinguishes a path never read from one only read partially, and
// names the whole-file read that enables editing (the run-10 worker looped on the
// generic "not read yet" message without realizing a ranged read does not qualify).
func (r *readRegistry) checkWritable(path string, currentMtimeMs int64) (bool, string) {
	if r == nil {
		return true, ""
	}
	r.mu.Lock()
	mark, ok := r.m[absPath(path)]
	r.mu.Unlock()
	if !ok {
		return false, "File has not been read yet. Read it whole (fs.read with no offset/limit) before writing or editing it."
	}
	if !mark.full {
		return false, "Only a partial/ranged read of this file is on record, which does not enable editing. Do a whole-file fs.read (no offset/limit) first, then edit."
	}
	if currentMtimeMs > mark.mtimeMs {
		return false, "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."
	}
	return true, ""
}
