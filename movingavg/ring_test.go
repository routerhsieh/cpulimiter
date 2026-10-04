package movingavg

import (
	"math/rand/v2"
	"strconv"
	"testing"
)

func TestRingMatchesHistoryModelAcrossResizes(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	capacity := 3
	r := New(capacity)
	var history []float64
	for step := 0; step < 1000; step++ {
		if random.IntN(3) == 0 {
			capacity = 1 + random.IntN(20)
			r.Resize(capacity)
		} else {
			v := float64(random.IntN(201) - 100)
			r.Add(v)
			history = append(history, v)
		}
		if len(history) > capacity {
			history = history[len(history)-capacity:]
		}
		var want float64
		for _, v := range history {
			want += v
		}
		if len(history) > 0 {
			want /= float64(len(history))
		}
		if got := r.Avg(); got != want {
			t.Fatalf("step %d: average=%g, want %g from %v", step, got, want, history)
		}
	}
}

func TestNewRejectsInvalidCapacity(t *testing.T) {
	for _, n := range []int{0, -1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic for invalid capacity")
				}
			}()
			New(n)
		})
	}
}

func TestResizePreservesNewestSamples(t *testing.T) {
	tests := []struct {
		name        string
		capacity    int
		samples     []float64
		newCapacity int
		want        float64
		more        []float64
		wantAfter   float64
	}{
		{"empty", 3, nil, 2, 0, []float64{4}, 4},
		{"grow partial", 4, []float64{1, 2}, 6, 1.5, []float64{3, 4, 5, 6, 7}, 4.5},
		{"grow wrapped", 3, []float64{1, 2, 3, 4, 5}, 5, 4, []float64{6, 7, 8}, 6},
		{"shrink partial", 5, []float64{1, 2, 3}, 2, 2.5, []float64{4}, 3.5},
		{"shrink wrapped", 4, []float64{1, 2, 3, 4, 5, 6}, 2, 5.5, []float64{7}, 6.5},
		{"shrink to one", 3, []float64{1, 2, 3, 4}, 1, 4, []float64{5}, 5},
		{"same capacity", 3, []float64{1, 2, 3, 4}, 3, 3, []float64{5}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(tt.capacity)
			for _, v := range tt.samples {
				r.Add(v)
			}
			r.Resize(tt.newCapacity)
			if got := r.Avg(); got != tt.want {
				t.Fatalf("average after resize = %v, want %v", got, tt.want)
			}
			for _, v := range tt.more {
				r.Add(v)
			}
			if got := r.Avg(); got != tt.wantAfter {
				t.Fatalf("average after adding = %v, want %v", got, tt.wantAfter)
			}
		})
	}
}

func TestResizeRejectsInvalidCapacity(t *testing.T) {
	for _, n := range []int{0, -1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			r := New(2)
			r.Add(3)
			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected panic")
					}
				}()
				r.Resize(n)
			}()
			if r.Avg() != 3 || r.size != 2 {
				t.Fatal("invalid resize changed the ring")
			}
		})
	}
}
