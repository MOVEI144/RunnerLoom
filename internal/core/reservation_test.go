package core

import (
	"context"
	"database/sql"
	"testing"
)

// CPU/RAM returned by a stopped VM still belong to its hard-reserved slot
// until that slot's disk cleanup finishes. General workloads must not borrow it.
func TestReservationSurvivesPartialCleanup(t *testing.T) {
	s, c := testStore(t)
	observe(t, s, c, 1)
	general := allocate(t, s, "linux-lite")
	dedicated := allocate(t, s, "linux-heavy")
	observe(t, s, c, 2,
		VMReport{ID: general.ID, State: "Unknown"},
		VMReport{ID: dedicated.ID, State: "Stopped"})
	if _, e := s.Allocate(ctx, ID(), "linux-lite"); code(e) != "NO_CAPACITY" {
		t.Fatalf("partially cleaned dedicated slot leaked into general capacity: %v", e)
	}
	// The slot becomes reusable for its owner only after all resources return.
	observe(t, s, c, 3,
		VMReport{ID: general.ID, State: "Unknown"},
		VMReport{ID: dedicated.ID, State: "Deleted"})
	replacement := allocate(t, s, "linux-heavy")
	if replacement.Reservation != dedicated.Reservation {
		t.Fatal("lost reservation ownership")
	}
}

func TestReservationCannotMoveOrRetargetWhileHeld(t *testing.T) {
	for _, change := range []string{"node", "pool"} {
		t.Run(change, func(t *testing.T) {
			s, c := testStore(t)
			observe(t, s, c, 1)
			_ = allocate(t, s, "linux-heavy")
			if change == "node" {
				n := c.Nodes[0]
				n.Name = "node-b"
				c.Nodes = append(c.Nodes, n)
				c.Reservations[0].Node = n.Name
			} else {
				c.Reservations[0].Pool = "linux-lite"
			}
			p, e := s.Plan(ctx, c)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.Apply(ctx, p.ID); code(e) != "RESERVATION_BUSY" {
				t.Fatalf("in-use reservation changed its owner: %v", e)
			}
		})
	}
}

func TestPartiallyReleasedReservationPreventsBudgetShrink(t *testing.T) {
	s, c := testStore(t)
	observe(t, s, c, 1)
	general := allocate(t, s, "linux-lite")
	dedicated := allocate(t, s, "linux-heavy")
	observe(t, s, c, 2, VMReport{ID: general.ID, State: "Unknown"}, VMReport{ID: dedicated.ID, State: "Stopped"})
	c.Nodes[0].Budget.CPU = 8 // enough for the reservation alone, but not it plus general
	p, e := s.Plan(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(ctx, p.ID); code(e) != "BUDGET_BUSY" {
		t.Fatalf("budget shrink discarded protected reservation: %v", e)
	}
}

func TestReservationCorruptionBlocksNewAllocation(t *testing.T) {
	s, c := testStore(t)
	observe(t, s, c, 1)
	a := allocate(t, s, "linux-heavy")
	// Simulates a damaged/restored database; do not silently release capacity.
	a.Node = "node-that-no-longer-exists"
	if e := s.transaction(context.Background(), func(tx *sql.Tx) error { return saveInstance(ctx, tx, a) }); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Allocate(ctx, ID(), "linux-lite"); code(e) != "NO_CAPACITY" {
		t.Fatalf("corrupted reservation reference accepted: %v", e)
	}
}
