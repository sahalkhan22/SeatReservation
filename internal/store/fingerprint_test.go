package store

import "testing"

func TestFingerprintIsOrderIndependent(t *testing.T) {
	// ["A1","A2"] and ["A2","A1"] are the same request, so a client that
	// retries with its seats shuffled must still be recognised as a replay.
	if fingerprint([]string{"A1", "A2"}) != fingerprint([]string{"A2", "A1"}) {
		t.Fatal("seat order must not change the fingerprint")
	}
}

func TestFingerprintDistinguishesDifferentSeatSets(t *testing.T) {
	if fingerprint([]string{"A1"}) == fingerprint([]string{"A2"}) {
		t.Fatal("different seats must produce different fingerprints")
	}
	if fingerprint([]string{"A1"}) == fingerprint([]string{"A1", "A2"}) {
		t.Fatal("a superset must produce a different fingerprint")
	}
}

func TestFingerprintDoesNotMutateInput(t *testing.T) {
	seats := []string{"A2", "A1"}
	fingerprint(seats)
	if seats[0] != "A2" {
		t.Fatalf("fingerprint sorted the caller's slice in place: %v", seats)
	}
}
