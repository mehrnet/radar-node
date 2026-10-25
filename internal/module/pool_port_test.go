package module

import "testing"

func TestAllocateUniquePortSkipsPinned(t *testing.T) {
	used := map[int]bool{100: true, 101: true}
	sequence := []int{100, 101, 102}
	next := 0
	port, err := allocateUniquePort(used, func() (int, error) {
		p := sequence[next]
		next++
		return p, nil
	})
	if err != nil {
		t.Fatalf("allocateUniquePort: %v", err)
	}
	if port != 102 {
		t.Fatalf("port = %d, want 102", port)
	}
}

func TestAllocateUniquePortGivesUpWhenExhausted(t *testing.T) {
	_, err := allocateUniquePort(map[int]bool{100: true}, func() (int, error) {
		return 100, nil
	})
	if err == nil {
		t.Fatal("expected an error when every allocation collides")
	}
}
