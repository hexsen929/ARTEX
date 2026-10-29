package server

import (
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestWorkerSlotsForTaskCombinesOverridesAndModelPools(t *testing.T) {
	pools := []WorkerPool{{ProfileID: 7, Workers: 2}, {ProfileID: 12, Workers: 1}}
	for _, tc := range []struct {
		name     string
		task     *Task
		pools    []WorkerPool
		global   int
		profiles []int64
	}{
		{"task inherits model pools", &Task{}, pools, 9, []int64{7, 7, 12}},
		{"task override replaces global pools", &Task{WorkerCount: 2}, pools, 9, []int64{0, 0}},
		{"task inherits legacy count", &Task{}, nil, 4, []int64{0, 0, 0, 0}},
		{"task override without pools", &Task{WorkerCount: 1}, nil, 4, []int64{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, err := workerSlotsForTask(tc.task, tc.pools, tc.global)
			if err != nil || len(slots) != len(tc.profiles) {
				t.Fatalf("slots=%+v err=%v, want profiles %v", slots, err, tc.profiles)
			}
			for i, profile := range tc.profiles {
				if slots[i].ProfileID != profile || slots[i].Name != fmt.Sprintf("work#%d", i+1) {
					t.Fatalf("slot[%d]=%+v, want profile %d", i, slots[i], profile)
				}
			}
		})
	}
	for _, count := range []int{-1, db.MaxTaskWorkerCount + 1} {
		if _, err := workerSlotsForTask(&Task{WorkerCount: count}, pools, 3); err == nil {
			t.Fatalf("invalid task override %d was silently replaced", count)
		}
	}
	for _, count := range []int{0, -1, db.MaxTaskWorkerCount + 1} {
		if _, err := workerSlotsForTask(&Task{}, nil, count); err == nil {
			t.Fatalf("invalid legacy count %d was silently replaced", count)
		}
	}

	if _, err := workerSlotsForTask(&Task{}, []WorkerPool{{ProfileID: 0, Workers: 1}}, 3); err == nil {
		t.Fatal("invalid inherited pool accepted")
	}
}
