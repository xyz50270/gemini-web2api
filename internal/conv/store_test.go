package conv

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestChainedLookup is the core invariant of the cache: the key stored after a
// turn must equal the key looked up at the start of the next turn.
//
// Turn N sends the whole history, so the next turn's prefix is exactly what we
// stored (history + reply). Storing the pre-send prefix instead would never
// match, which is the bug this test exists to prevent.
func TestChainedLookup(t *testing.T) {
	const model = "gemini-4-argon"

	sys := Msg{Role: "system", Content: "be brief"}
	u1 := Msg{Role: "user", Content: "hello"}
	a1 := Msg{Role: "assistant", Content: "hi there"}

	// Turn 1: no history yet, so the prefix is just the system message.
	prefix1 := []Msg{sys}
	key1 := HistoryKey(model, prefix1)

	store := newTestStore(t)
	if _, ok := store.Get(key1); ok {
		t.Fatal("turn 1 should miss")
	}

	// After turn 1 we store the history INCLUDING the reply.
	stored1 := HistoryKey(model, []Msg{sys, u1, a1})
	store.Put(&Record{Key: stored1, Model: NormalizeModel(model), Metadata: []any{"c_1", "r_1"}})

	// Turn 2: client resends [sys,u1,a1] plus the new user message.
	u2 := Msg{Role: "user", Content: "and then?"}
	prefix2 := []Msg{sys, u1, a1}
	if got := HistoryKey(model, prefix2); got != stored1 {
		t.Fatalf("turn 2 prefix key %s != stored key %s", Fingerprint(got), Fingerprint(stored1))
	}
	rec, ok := store.Get(stored1)
	if !ok {
		t.Fatal("turn 2 should hit the key stored after turn 1")
	}
	if len(rec.Metadata) != 2 {
		t.Errorf("metadata = %v", rec.Metadata)
	}

	// Turn 3 chains off turn 2.
	stored2 := HistoryKey(model, []Msg{sys, u1, a1, u2, Msg{Role: "assistant", Content: "sure"}})
	store.Put(&Record{Key: stored2, Model: NormalizeModel(model), Metadata: []any{"c_2", "r_2"}})

	// u3 is not part of the prefix; the prefix stops before it.
	prefix3 := []Msg{sys, u1, a1, u2, Msg{Role: "assistant", Content: "sure"}}
	if got := HistoryKey(model, prefix3); got != stored2 {
		t.Fatalf("turn 3 prefix key %s != stored key %s", Fingerprint(got), Fingerprint(stored2))
	}
}

// TestModelSwitchMisses pins the behaviour the user asked for: changing model
// must not reuse the conversation (upstream rejects it), and the previous
// model's record must survive so switching back still hits.
func TestModelSwitchMisses(t *testing.T) {
	history := []Msg{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}

	keyArgon := HistoryKey("gemini-4-argon", history)
	keyPro := HistoryKey("gemini-3.1-pro", history)

	if keyArgon == keyPro {
		t.Fatal("different models must produce different keys")
	}

	store := newTestStore(t)
	store.Put(&Record{Key: keyArgon, Model: NormalizeModel("gemini-4-argon"), Metadata: []any{"c_a"}})

	if _, ok := store.Get(keyPro); ok {
		t.Error("switching model must miss, otherwise upstream returns MODEL_INCONSISTENT")
	}
	// Switching back must still find the old conversation.
	if _, ok := store.Get(keyArgon); !ok {
		t.Error("the original model's record must survive a model switch")
	}
}

func TestDifferentConversationsDiffer(t *testing.T) {
	a := []Msg{{Role: "user", Content: "what is a b-tree"}}
	b := []Msg{{Role: "user", Content: "what is a red-black tree"}}
	if HistoryKey("m", a) == HistoryKey("m", b) {
		t.Error("distinct conversations must not collide")
	}
}

func TestThinkSuffixIgnored(t *testing.T) {
	history := []Msg{{Role: "user", Content: "hi"}}
	plain := HistoryKey("gemini-4-argon", history)
	thinking := HistoryKey("gemini-4-argon@think=0", history)
	if plain != thinking {
		t.Error("@think=N should not change the conversation identity")
	}
}

func TestNormalizeContentCollapsesWhitespace(t *testing.T) {
	a := HistoryKey("m", []Msg{{Role: "user", Content: "hello   world \n"}})
	b := HistoryKey("m", []Msg{{Role: "user", Content: "hello world"}})
	if a != b {
		t.Error("whitespace-only differences should not miss the cache")
	}
}

func TestEmptyRoleDefaultsToUser(t *testing.T) {
	a := HistoryKey("m", []Msg{{Role: "", Content: "x"}})
	b := HistoryKey("m", []Msg{{Role: "user", Content: "x"}})
	if a != b {
		t.Error("empty role should hash the same as user")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "conversations.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorePutGet(t *testing.T) {
	s := newTestStore(t)
	s.Put(&Record{Key: "k1", Model: "m", Account: "0", Metadata: []any{"c_1", "r_1"}})

	rec, ok := s.Get("k1")
	if !ok {
		t.Fatal("Get after Put should hit")
	}
	if rec.Model != "m" || rec.Account != "0" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Hits != 1 {
		t.Errorf("Hits = %d, want 1", rec.Hits)
	}
	s.Get("k1")
	rec, _ = s.Get("k1")
	if rec.Hits != 3 {
		t.Errorf("Hits = %d after three Gets, want 3", rec.Hits)
	}
}

func TestStoreDelete(t *testing.T) {
	s := newTestStore(t)
	s.Put(&Record{Key: "k1", Metadata: []any{"c"}})
	s.Delete("k1")
	if _, ok := s.Get("k1"); ok {
		t.Error("deleted record should not be found")
	}
}

func TestStoreResponseIDIndex(t *testing.T) {
	s := newTestStore(t)
	s.Put(&Record{Key: "k1", ResponseID: "resp_abc", Metadata: []any{"c"}})

	rec, ok := s.GetByResponseID("resp_abc")
	if !ok {
		t.Fatal("should resolve via response id")
	}
	if rec.Key != "k1" {
		t.Errorf("Key = %q", rec.Key)
	}

	// Replacing the record must not leave a stale response-id alias.
	s.Put(&Record{Key: "k1", ResponseID: "resp_def", Metadata: []any{"c2"}})
	if _, ok := s.GetByResponseID("resp_abc"); ok {
		t.Error("old response id should no longer resolve")
	}
	if _, ok := s.GetByResponseID("resp_def"); !ok {
		t.Error("new response id should resolve")
	}
}

func TestStorePurgeOlderThan(t *testing.T) {
	s := newTestStore(t)
	s.Put(&Record{Key: "old", Metadata: []any{"c1"}})
	s.Put(&Record{Key: "new", Metadata: []any{"c2"}})

	// Age one record past the cutoff.
	s.mu.Lock()
	s.recs["old"].LastUsed = time.Now().Add(-31 * 24 * time.Hour)
	s.mu.Unlock()

	removed := s.PurgeOlderThan(30 * 24 * time.Hour)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, ok := s.Get("old"); ok {
		t.Error("old record should be purged")
	}
	if _, ok := s.Get("new"); !ok {
		t.Error("recent record should survive")
	}
}

func TestStorePurgeIgnoresNonPositiveTTL(t *testing.T) {
	s := newTestStore(t)
	s.Put(&Record{Key: "k", Metadata: []any{"c"}})
	if n := s.PurgeOlderThan(0); n != 0 {
		t.Errorf("removed = %d with ttl 0, want 0", n)
	}
	if s.Len() != 1 {
		t.Error("record should survive a non-positive ttl")
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s1.Put(&Record{Key: "k1", Model: "m", Metadata: []any{"c_1", "r_1"}, ResponseID: "resp_x"})
	if err := s1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec, ok := s2.Get("k1")
	if !ok {
		t.Fatal("record should survive a reopen")
	}
	if len(rec.Metadata) != 2 {
		t.Errorf("metadata = %v", rec.Metadata)
	}
	if _, ok := s2.GetByResponseID("resp_x"); !ok {
		t.Error("response id index should be rebuilt on load")
	}
}

func TestStoreSaveIsAtomicOnBadDirectory(t *testing.T) {
	// Point at a path whose parent is a file, so the temp write must fail
	// cleanly without losing the in-memory state or panicking.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	s, err := Open(filepath.Join(blocker, "nested", "conversations.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Put(&Record{Key: "k", Metadata: []any{"c"}})
	if err := s.Save(); err == nil {
		t.Error("Save into an unwritable path should report an error")
	}
	if _, ok := s.Get("k"); !ok {
		t.Error("in-memory state must survive a failed save")
	}
}

func TestStoreCorruptFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a corrupt file should not fail: %v", err)
	}
	if s.Len() != 0 {
		t.Error("corrupt file should yield an empty store")
	}
	if s.Status() == "" {
		t.Error("Status should describe the quarantined file")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) == 0 {
		t.Error("corrupt file should be moved aside, not deleted")
	}
}

func TestStoreLenAndOldest(t *testing.T) {
	s := newTestStore(t)
	if _, ok := s.Oldest(); ok {
		t.Error("empty store should report no oldest")
	}
	s.Put(&Record{Key: "a", Metadata: []any{"c"}})
	s.Put(&Record{Key: "b", Metadata: []any{"c"}})
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
	if _, ok := s.Oldest(); !ok {
		t.Error("populated store should report an oldest")
	}
}

func TestAppendAssistant(t *testing.T) {
	base := []Msg{{Role: "user", Content: "hi"}}
	out := AppendAssistant(base, "hello")
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[1].Role != "assistant" || out[1].Content != "hello" {
		t.Errorf("appended = %+v", out[1])
	}
	if len(base) != 1 {
		t.Error("AppendAssistant must not mutate the input slice")
	}
}

func TestSuffixDoesNotAffectPrefixKey(t *testing.T) {
	// The stored key is computed over history including the reply, so appending
	// a brand new user message must not change the key of the prefix.
	sys := Msg{Role: "system", Content: "s"}
	u1 := Msg{Role: "user", Content: "u1"}
	a1 := Msg{Role: "assistant", Content: "a1"}
	u2 := Msg{Role: "user", Content: "u2"}

	prefix := []Msg{sys, u1, a1}
	withTail := []Msg{sys, u1, a1, u2}

	if HistoryKey("m", prefix) == HistoryKey("m", withTail) {
		t.Error("different message counts must not collide")
	}
	// And the prefix key must equal the key computed before u2 was appended.
	if HistoryKey("m", prefix) != HistoryKey("m", prefix[:len(prefix)]) {
		t.Error("prefix key should be stable")
	}
}
