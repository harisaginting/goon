// Package usage is goon's token-usage meter.
//
// LLM providers report token counts here after each Generate call via the
// process-wide Global() meter. Totals are aggregated per model and persisted
// to ./storage/usage.json so the dashboard can show "how many tokens has goon
// spent, broken down by model" across daemon restarts.
//
// Design notes:
//   - Stdlib only (project rule). No external metrics lib.
//   - Best-effort persistence: a write failure is logged-by-omission, never
//     propagated — losing a usage tick must never disturb an LLM call.
//   - Importable from the low-level llm package without a cycle: usage only
//     depends on internal/storage.
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/storage"
)

// ModelStat is the running total for a single model.
type ModelStat struct {
	Model            string `json:"model"`
	Calls            int64  `json:"calls"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// TotalTokens is prompt + completion.
func (s ModelStat) TotalTokens() int64 { return s.PromptTokens + s.CompletionTokens }

// storeFile is the on-disk shape of usage.json.
type storeFile struct {
	Models    map[string]*ModelStat `json:"models"`
	UpdatedAt time.Time             `json:"updated_at,omitempty"`
}

// Meter accumulates per-model token usage and persists it.
type Meter struct {
	mu    sync.Mutex
	path  string
	store storeFile
}

// New opens (or creates) a Meter backed by the given JSON file. An empty path
// falls back to <storage.Root()>/usage.json. Existing data is loaded; a
// missing or unreadable file starts empty.
func New(path string) *Meter {
	if path == "" {
		path = storage.Path("usage.json")
	}
	m := &Meter{path: path, store: storeFile{Models: map[string]*ModelStat{}}}
	if b, err := os.ReadFile(path); err == nil {
		var disk storeFile
		if json.Unmarshal(b, &disk) == nil && disk.Models != nil {
			m.store = disk
		}
	}
	return m
}

var (
	globalOnce sync.Once
	global     *Meter
)

// Global returns the process-wide meter, created on first use.
func Global() *Meter {
	globalOnce.Do(func() { global = New("") })
	return global
}

// Record adds one call's token counts to the model's running total and flushes
// to disk. Negative counts are clamped to zero; an empty model name is stored
// as "unknown" so usage is never silently dropped. Safe for concurrent use.
func (m *Meter) Record(model string, prompt, completion int) {
	if m == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.Models == nil {
		m.store.Models = map[string]*ModelStat{}
	}
	st := m.store.Models[model]
	if st == nil {
		st = &ModelStat{Model: model}
		m.store.Models[model] = st
	}
	st.Calls++
	st.PromptTokens += int64(prompt)
	st.CompletionTokens += int64(completion)
	m.store.UpdatedAt = time.Now()
	m.flush()
}

// Snapshot returns per-model stats sorted by total tokens descending (busiest
// model first). Safe for concurrent use.
func (m *Meter) Snapshot() []ModelStat {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelStat, 0, len(m.store.Models))
	for _, st := range m.store.Models {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens() != out[j].TotalTokens() {
			return out[i].TotalTokens() > out[j].TotalTokens()
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Totals returns the summed calls / prompt / completion across all models.
func (m *Meter) Totals() (calls, prompt, completion int64) {
	if m == nil {
		return 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.store.Models {
		calls += st.Calls
		prompt += st.PromptTokens
		completion += st.CompletionTokens
	}
	return calls, prompt, completion
}

// UpdatedAt reports when usage was last recorded (zero if never).
func (m *Meter) UpdatedAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.UpdatedAt
}

// flush writes the store to disk atomically (tmp file + rename). Best-effort;
// caller holds m.mu.
func (m *Meter) flush() {
	if m.path == "" {
		return
	}
	b, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(m.path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp, err := os.CreateTemp(dir, ".usage.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, m.path)
}
