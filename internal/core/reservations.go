package core

// A reservation is identified by its name AND its node/pool owner. Moving a
// live reservation would make the same slot appear consumed on the wrong node.
func validateReservationOwners(c Config, runs []Instance) error {
	byName := make(map[string]Reservation, len(c.Reservations))
	for _, r := range c.Reservations {
		byName[r.Name] = r
	}
	for _, a := range runs {
		if a.Reservation == "" || a.Held.Empty() {
			continue
		}
		r, ok := byName[a.Reservation]
		if !ok || r.Node != a.Node || r.Pool != a.Pool.Name {
			return Fail("RESERVATION_BUSY", "使用中の予約のNode・Poolを変更できません。片付け完了を待ってください", a.Reservation)
		}
	}
	return nil
}

// reservationProtection returns the reserved resources not already accounted
// for in Held, and the number of completely unused slots. Partially released
// slots protect their returned components, but cannot be allocated again until
// their remaining disk/GPU cleanup is confirmed. This keeps one resource ledger
// without lending a dedicated pool's idle capacity to general jobs.
func reservationProtection(c Config, r Reservation, runs []Instance) (Resources, int64, error) {
	p, ok := c.Pool(r.Pool)
	if !ok {
		return Resources{}, 0, Fail("RESERVATION_INCONSISTENT", "予約のPoolがありません", r.Name)
	}
	used := int64(0)
	held := Resources{}
	for _, a := range runs {
		if a.Reservation != r.Name || a.Held.Empty() {
			continue
		}
		if a.Node != r.Node || a.Pool.Name != r.Pool || !a.Held.Fits(p.Charge()) {
			return Resources{}, 0, Fail("RESERVATION_INCONSISTENT", "予約台帳と実行記録が一致しません", r.Name)
		}
		used++
		held = held.Add(a.Held)
	}
	if used > r.Slots {
		return Resources{}, 0, Fail("RESERVATION_BUSY", "使用中の予約を減らせません", r.Name)
	}
	return p.Charge().Mul(r.Slots).Sub(held), r.Slots - used, nil
}
