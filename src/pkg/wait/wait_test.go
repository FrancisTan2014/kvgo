package wait

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitRegisterAndTrigger_036o(t *testing.T) {
	w := New()
	ch := w.Register(1)

	go func() {
		w.Trigger(1, "ok")
	}()

	select {
	case v := <-ch:
		require.Equal(t, "ok", v)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for trigger")
	}
}

func TestWaitTriggerUnregisteredIdIsNoop_036o(t *testing.T) {
	w := New()
	// must not panic
	w.Trigger(999, "ignored")
}
