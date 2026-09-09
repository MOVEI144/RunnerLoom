package core

import "testing"

// Exercise every mixture of unused, running, stopped and fully deleted slots.
// The protected slot capacity plus its held resources must remain constant.
func FuzzReservationProtection(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{2, 2, 2, 2})
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, states []byte) {
		if len(states) == 0 || len(states) > 32 {
			return
		}
		c := Example()
		p := c.Pools[1]
		r := c.Reservations[0]
		r.Slots = int64(len(states))
		runs := []Instance{}
		held := Resources{}
		unused := int64(0)
		for _, state := range states {
			a := Instance{Node: r.Node, Pool: p, Reservation: r.Name}
			switch state % 4 {
			case 0:
				unused++
				continue
			case 1:
				a.Held = p.Charge()
			case 2:
				a.Held = Resources{Disk: p.Charge().Disk}
			case 3:
				a.State = "Deleted"
				unused++
			}
			held = held.Add(a.Held)
			runs = append(runs, a)
		}
		protected, slots, e := reservationProtection(c, r, runs)
		if e != nil {
			t.Fatal(e)
		}
		if slots != unused || protected.Add(held) != p.Charge().Mul(r.Slots) {
			t.Fatalf("reservation invariant broken: protected=%+v held=%+v slots=%d expected=%d", protected, held, slots, unused)
		}
	})
}
