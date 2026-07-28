package httputil_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/httputil"
)

type childKey struct{}

// TestRecordActorVisibleThroughParentContext exercises the property the holder
// exists for: the request logger installs the holder on the OUTERMOST context,
// auth middleware records the actor on a derived child context (several
// middlewares deeper), and the logger must still see the recorded value when it
// reads back through its own, older context. Plain context.WithValue cannot do
// this — values only flow inward — which is why RecordActor writes through a
// mutable holder instead of deriving a new context.
func TestRecordActorVisibleThroughParentContext(t *testing.T) {
	ctx := httputil.WithActorHolder(context.Background())

	child := context.WithValue(ctx, childKey{}, "deeper")
	httputil.RecordActor(child, "nicole_tyrfing", "key")

	name, kind, ok := httputil.RecordedActor(ctx)
	if !ok {
		t.Fatal("RecordedActor ok = false, want true after RecordActor on child context")
	}
	if name != "nicole_tyrfing" || kind != "key" {
		t.Errorf("RecordedActor = (%q, %q), want (nicole_tyrfing, key)", name, kind)
	}
}

// TestRecordActorWithoutHolderIsNoop pins the no-holder behavior: surfaces that
// aren't wrapped by the request logger (tests hitting a bare router, future
// listeners) call RecordActor on a context with no holder, and that must be a
// silent no-op — never a panic, never an implicit holder.
func TestRecordActorWithoutHolderIsNoop(t *testing.T) {
	httputil.RecordActor(context.Background(), "nicole_tyrfing", "key")

	if _, _, ok := httputil.RecordedActor(context.Background()); ok {
		t.Error("RecordedActor ok = true on a context with no holder, want false")
	}
}
