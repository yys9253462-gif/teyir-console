package collector

import "testing"

func TestTrafficDifferencePerDirectionReset(t *testing.T) {
	old := trafficCursor{Epoch: "e1", Counters: map[string]trafficCounter{"u": {Tx: 100, Rx: 200}}}
	current := trafficCursor{Epoch: "e1", Counters: map[string]trafficCounter{"u": {Tx: 80, Rx: 250}}}
	rx, tx, next, err := trafficDifference(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if tx != 0 {
		t.Fatalf("tx reset should contribute 0, got %d", tx)
	}
	if rx != 50 {
		t.Fatalf("rx increment should survive peer reset, got %d", rx)
	}
	if next.Counters["u"] != (trafficCounter{Tx: 80, Rx: 250}) {
		t.Fatalf("cursor not advanced: %+v", next.Counters["u"])
	}
}

func TestTrafficDifferenceBothReset(t *testing.T) {
	old := trafficCursor{Epoch: "e1", Counters: map[string]trafficCounter{"u": {Tx: 100, Rx: 200}}}
	current := trafficCursor{Epoch: "e1", Counters: map[string]trafficCounter{"u": {Tx: 10, Rx: 20}}}
	rx, tx, _, err := trafficDifference(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if rx != 0 || tx != 0 {
		t.Fatalf("both resets should zero deltas, got rx=%d tx=%d", rx, tx)
	}
}
