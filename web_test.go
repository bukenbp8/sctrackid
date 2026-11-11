package main

import "testing"

func TestRetriesRunFirst(t *testing.T) {
	s := &server{jobs: []*job{
		{ID: 1, State: stateRunning},
		{ID: 2, State: stateQueued},
		{ID: 3, State: stateQueued, Req: jobRequest{Retry: &retryRef{}}},
		{ID: 4, State: stateQueued},
		{ID: 5, State: stateQueued, Req: jobRequest{Retry: &retryRef{}}},
	}}
	var order []int
	for _, j := range s.queuedLocked() {
		order = append(order, j.ID)
	}
	if len(order) != 4 || order[0] != 3 || order[1] != 5 || order[2] != 2 || order[3] != 4 {
		t.Errorf("run order = %v", order)
	}
	pos := map[int]int{}
	for _, j := range s.snapshotLocked() {
		pos[j.ID] = j.Position
	}
	if pos[1] != 0 || pos[3] != 1 || pos[5] != 2 || pos[2] != 3 || pos[4] != 4 {
		t.Errorf("positions = %v", pos)
	}
}
