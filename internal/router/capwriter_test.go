package router

import (
	"bytes"
	"errors"
	"testing"
)

// TestMaxHandlerOutputIsPinned spells the cap out literally, because every
// other cap test sizes its fixture from the symbol and so would follow the
// constant anywhere it moved. §5 fixes this number; changing it is a design
// decision, not an edit.
func TestMaxHandlerOutputIsPinned(t *testing.T) {
	if MaxHandlerOutput != 65536 {
		t.Errorf("MaxHandlerOutput = %d, want 65536 (§5's 64 KiB stdout cap)", MaxHandlerOutput)
	}
}

func TestCapWriterKillsOnTheFirstByteBeyondTheCap(t *testing.T) {
	kills := 0
	w := &capWriter{limit: 8, kill: func() { kills++ }}

	if n, err := w.Write([]byte("abcde")); n != 5 || err != nil {
		t.Fatalf("first Write = (%d, %v), want (5, nil)", n, err)
	}
	if kills != 0 {
		t.Fatalf("killed %d times under the cap, want 0", kills)
	}

	n, err := w.Write([]byte("fghij"))
	if n != 3 || !errors.Is(err, errCapExceeded) {
		t.Errorf("overflowing Write = (%d, %v), want (3, %v)", n, err, errCapExceeded)
	}
	if kills != 1 {
		t.Errorf("killed %d times, want 1", kills)
	}

	if n, err := w.Write([]byte("k")); n != 0 || !errors.Is(err, errCapExceeded) {
		t.Errorf("Write after overflow = (%d, %v), want (0, %v)", n, err, errCapExceeded)
	}
	if kills != 1 {
		t.Errorf("killed %d times in total, want 1: the kill fires once", kills)
	}

	got, overflowed := w.collected()
	if !overflowed {
		t.Error("collected reported no overflow")
	}
	if !bytes.Equal(got, []byte("abcdefgh")) {
		t.Errorf("collected %q, want exactly the first %d bytes", got, w.limit)
	}
}

func TestCapWriterExactlyAtCapDoesNotKill(t *testing.T) {
	kills := 0
	w := &capWriter{limit: 8, kill: func() { kills++ }}

	if n, err := w.Write(bytes.Repeat([]byte("a"), 8)); n != 8 || err != nil {
		t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
	}
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Errorf("empty Write at the cap = (%d, %v), want (0, nil)", n, err)
	}
	if kills != 0 {
		t.Errorf("killed %d times, want 0: exactly at the cap is not past it", kills)
	}

	got, overflowed := w.collected()
	if overflowed || len(got) != 8 {
		t.Errorf("collected %d bytes, overflowed=%v, want 8 and false", len(got), overflowed)
	}
}
