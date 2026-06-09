package learnings

import (
	"context"
	"testing"
	"time"
)

func TestReflectInterval(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("GOON_LEARN_INTERVAL_HOURS", "")
		if got := ReflectInterval(); got != defaultReflectInterval {
			t.Errorf("got %v, want %v", got, defaultReflectInterval)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("GOON_LEARN_INTERVAL_HOURS", "6")
		if got := ReflectInterval(); got != 6*time.Hour {
			t.Errorf("got %v, want 6h", got)
		}
	})
	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv("GOON_LEARN_INTERVAL_HOURS", "nonsense")
		if got := ReflectInterval(); got != defaultReflectInterval {
			t.Errorf("got %v, want default", got)
		}
	})
	t.Run("zero falls back", func(t *testing.T) {
		t.Setenv("GOON_LEARN_INTERVAL_HOURS", "0")
		if got := ReflectInterval(); got != defaultReflectInterval {
			t.Errorf("got %v, want default", got)
		}
	})
}

func TestReflectEnabled(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		t.Setenv("GOON_AUTO_LEARN", "")
		if !ReflectEnabled() {
			t.Error("expected enabled by default")
		}
	})
	t.Run("disabled via off value", func(t *testing.T) {
		t.Setenv("GOON_AUTO_LEARN", "off")
		if ReflectEnabled() {
			t.Error("expected disabled when GOON_AUTO_LEARN=off")
		}
	})
}

// TestReflect_NoDepsIsNoop ensures Reflect degrades safely (returns nil,
// no panic) when the LLM/tools/executor stack isn't wired — the daemon must
// never crash on an idle reflection.
func TestReflect_NoDepsIsNoop(t *testing.T) {
	if err := Reflect(context.Background(), ReflectOptions{}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}
