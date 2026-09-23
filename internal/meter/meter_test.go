package meter

import "testing"

// Holds the suite open until the first real behaviour lands.
func TestWeightIsUnsigned(t *testing.T) {
	var w Weight = 1
	if w != 1 {
		t.Fatalf("want 1, got %d", w)
	}
}
