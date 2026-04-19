package wait

import "testing"

func TestWaitTimeFastPath_037l(t *testing.T) {
	wt := NewTimeList()
	wt.Trigger(5)

	ch := wt.Wait(4)

	select {
	case <-ch:
		// ok — returned an already-closed channel
	default:
		t.Fatal("Wait on an index already passed by Trigger must return a closed channel")
	}
}

func TestWaitTimePartialTrigger_037l(t *testing.T) {
	wt := NewTimeList()
	ch3 := wt.Wait(3)
	ch5 := wt.Wait(5)
	ch7 := wt.Wait(7)

	wt.Trigger(5)

	// ch3 (index 3 <= 5) must be closed
	select {
	case <-ch3:
	default:
		t.Fatal("Trigger(5) must close channel at index 3")
	}

	// ch5 (index 5 <= 5) must be closed
	select {
	case <-ch5:
	default:
		t.Fatal("Trigger(5) must close channel at index 5")
	}

	// ch7 (index 7 > 5) must still be open
	select {
	case <-ch7:
		t.Fatal("Trigger(5) must not close channel at index 7")
	default:
		// ok — still blocking
	}
}
