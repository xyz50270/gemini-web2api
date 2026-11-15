// Package conv stores the server-side conversation handles Gemini hands back,
// so a follow-up turn can continue the same chat instead of resending the whole
// message history.
package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is one cached conversation turn.
type Record struct {
	// Key is derived from the model plus the full history that produced this
	// handle. Including the model means switching model always misses, which is
	// required: upstream rejects a conversation continued under another model.
	Key       string    `json:"key"`
	Model     string    `json:"model"`
	Account   string    `json:"account"`
	Metadata  []any     `json:"metadata"`
	ResponseID string   `json:"response_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
	Hits      int       `json:"hits"`
}

// Store is a small JSON-backed conversation cache.
//
// Writes are batched: Put only touches memory and marks the store dirty, and a
// background goroutine (or Close) flushes to disk. A service handling a handful
// of conversations does not need a database, and keeping the dependency list at
// zero avoids bloating a static binary.
type Store struct {
	path string

	mu    sync.RWMutex
	recs  map[string]*Record
	dirty bool
	// byResponseID indexes records by the id handed to /v1/responses clients so
	// previous_response_id can resume a chat.
	byResponseID map[string]string

	flushInterval time.Duration
	stop          chan struct{}
	stopOnce      sync.Once

	lastErr string
}

// Open loads the store from path, creating an empty one when the file is
// missing. A corrupt file is moved aside rather than fatal: losing the cache
// only costs tokens, it must never stop the service from starting.
func Open(path string) (*Store, error) {
	s := &Store{
		path:          path,
		recs:          make(map[string]*Record),
		byResponseID:  make(map[string]string),
		flushInterval: 30 * time.Second,
		stop:          make(chan struct{}),
	}
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}

	var loaded map[string]*Record
	if err := json.Unmarshal(data, &loaded); err != nil {
		quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		_ = os.Rename(path, quarantine)
		s.lastErr = fmt.Sprintf("cache file was corrupt (%v); moved to %s", err, quarantine)
		return s, nil
	}

	for k, r := range loaded {
		if r == nil {
			continue
		}
		r.Key = k
		s.recs[k] = r
		if r.ResponseID != "" {
			s.byResponseID[r.ResponseID] = k
		}
	}
	return s, nil
}

// Path reports where the store persists to.
func (s *Store) Path() string { return s.path }

// Status returns a human-readable description of the last load problem, if any.
func (s *Store) Status() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Get looks up a record and bumps its hit counters.
func (s *Store) Get(key string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.recs[key]
	if !ok {
		return nil, false
	}
	r.Hits++
	r.LastUsed = time.Now()
	s.dirty = true

	cp := *r
	return &cp, true
}

// GetByResponseID resolves a previous_response_id to its record.
func (s *Store) GetByResponseID(id string) (*Record, bool) {
	s.mu.RLock()
	key, ok := s.byResponseID[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return s.Get(key)
}

// Put stores a record, replacing any previous entry for the same key.
func (s *Store) Put(r *Record) {
	if r == nil || r.Key == "" {
		return
	}
	now := time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.LastUsed = now

	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop a stale response-id index entry before overwriting.
	if old, ok := s.recs[r.Key]; ok && old.ResponseID != "" && old.ResponseID != r.ResponseID {
		delete(s.byResponseID, old.ResponseID)
	}
	s.recs[r.Key] = r
	if r.ResponseID != "" {
		s.byResponseID[r.ResponseID] = r.Key
	}
	s.dirty = true
}

// Delete removes one record.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[key]; ok {
		if r.ResponseID != "" {
			delete(s.byResponseID, r.ResponseID)
		}
		delete(s.recs, key)
		s.dirty = true
	}
}

// PurgeOlderThan drops records not used within d and returns how many went.
func (s *Store) PurgeOlderThan(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-d)

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for k, r := range s.recs {
		if r.LastUsed.Before(cutoff) {
			if r.ResponseID != "" {
				delete(s.byResponseID, r.ResponseID)
			}
			delete(s.recs, k)
			removed++
		}
	}
	if removed > 0 {
		s.dirty = true
	}
	return removed
}

// Len reports how many records are cached.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recs)
}

// Oldest returns the age of the least recently used record.
func (s *Store) Oldest() (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.recs) == 0 {
		return time.Time{}, false
	}
	oldest := time.Time{}
	for _, r := range s.recs {
		if oldest.IsZero() || r.LastUsed.Before(oldest) {
			oldest = r.LastUsed
		}
	}
	return oldest, true
}

// Save writes the cache to disk atomically. Writing to a temp file and renaming
// means a crash mid-write leaves the previous good copy intact.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]*Record, len(s.recs))
	for k, v := range s.recs {
		cp := *v
		snapshot[k] = &cp
	}
	s.dirty = false
	s.mu.Unlock()

	if s.path == "" {
		return nil
	}

	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.noteErr(err)
			return err
		}
	}

	// Sorted marshal keeps the file diff-friendly between runs.
	keys := make([]string, 0, len(snapshot))
	for k := range snapshot {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]*Record, len(snapshot))
	for _, k := range keys {
		ordered[k] = snapshot[k]
	}

	data, err := json.Marshal(ordered)
	if err != nil {
		s.noteErr(err)
		return err
	}

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		s.noteErr(err)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		s.noteErr(err)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		s.noteErr(err)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		s.noteErr(err)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		s.noteErr(err)
		return err
	}
	return nil
}

func (s *Store) noteErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

// StartBackground flushes periodically until Close is called.
func (s *Store) StartBackground() {
	go func() {
		ticker := time.NewTicker(s.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				_ = s.Save()
			}
		}
	}()
}

// Close stops the background flusher and writes any pending changes.
func (s *Store) Close() error {
	s.stopOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
	})
	return s.Save()
}
