package soc

import (
	"errors"
	"fmt"
)

// State is a snapshot of the estimator's internal state. It exists so the
// estimate can be inspected and persisted from outside the package.
type State struct {
	VehicleSoc        float64 `json:"vehicleSoc"`
	FetchedSoc        float64 `json:"fetchedSoc"`
	PrevSoc           float64 `json:"prevSoc"`
	PrevChargedEnergy float64 `json:"prevChargedEnergy"`
	ChargedEnergy     float64 `json:"chargedEnergy"`
	InitialSoc        float64 `json:"initialSoc"`
	InitialEnergy     float64 `json:"initialEnergy"`
	EnergyPerSocStep  float64 `json:"energyPerSocStep"`
	VirtualCapacity   float64 `json:"virtualCapacity"`
	Learned           bool    `json:"learned"`
}

// State returns a snapshot of the estimator's internal state
func (s *Estimator) State() State {
	return State{
		VehicleSoc:        s.vehicleSoc,
		FetchedSoc:        s.fetchedSoc,
		PrevSoc:           s.prevSoc,
		PrevChargedEnergy: s.prevChargedEnergy,
		ChargedEnergy:     s.chargedEnergy,
		InitialSoc:        s.initialSoc,
		InitialEnergy:     s.initialEnergy,
		EnergyPerSocStep:  s.energyPerSocStep,
		VirtualCapacity:   s.virtualCapacity,
		Learned:           s.learned,
	}
}

// SetSoc shifts the energy anchor so the estimate reads target percent.
//
// The estimate is recomputed from the fetched value on every poll
// (vehicleSoc = fetchedSoc + energyDelta/energyPerSocStep), so assigning
// vehicleSoc alone would not survive. Shifting prevChargedEnergy does:
//
//	prevChargedEnergy_new = prevChargedEnergy_old - (target - current) * energyPerSocStep
//
// prevSoc, initialSoc and initialEnergy are deliberately left untouched.
// prevSoc must keep matching the source value, otherwise socDelta != 0 sends
// the next poll into the rebase branch and drops the override immediately.
// initialSoc/initialEnergy anchor the upstream gradient learner.
//
// The source value is a hard floor. Shifting the anchor below it pushes
// prevChargedEnergy past the counter, so the next poll sees energyDelta < 0,
// takes the rebase branch and throws the override away - accepting such a
// target would answer success and revert within one poll. The vehicle's own
// reading is the one thing the estimator cannot argue with; only energy
// delivered at the charger can lift the estimate above it.
func (s *Estimator) SetSoc(target float64) error {
	if target < 0 || target > 100 {
		return fmt.Errorf("soc out of range: %.1f", target)
	}

	if s.energyPerSocStep <= 0 {
		return errors.New("no gradient available")
	}

	if target < s.fetchedSoc {
		return fmt.Errorf("soc estimate cannot be set below the value reported by the vehicle (%.1f%%): %.1f", s.fetchedSoc, target)
	}

	s.prevChargedEnergy -= (target - s.vehicleSoc) * s.energyPerSocStep
	s.vehicleSoc = target

	return nil
}

// ShiftEnergy moves the energy anchor by wh, raising the estimate for
// positive and lowering it for negative values.
func (s *Estimator) ShiftEnergy(wh float64) error {
	if s.energyPerSocStep <= 0 {
		return errors.New("no gradient available")
	}

	return s.SetSoc(s.vehicleSoc + wh/s.energyPerSocStep)
}

// ResetOverride drops the offset the estimate carries above the source value,
// so the estimate follows the vehicle again.
//
// Deleting the persisted record alone does not do this: the offset lives in
// the running estimator's prevChargedEnergy, and the next poll would simply
// write the unchanged estimate back into a fresh record. Re-anchoring on
// fetchedSoc is the exact inverse of SetSoc and needs no knowledge of the
// current counter - it moves prevChargedEnergy to the counter value of the
// last poll, so energy delivered from here on still lifts the estimate as it
// should. On an estimator that was restored but never polled, fetchedSoc is
// the restored anchor (see Restore), not zero.
func (s *Estimator) ResetOverride() error {
	return s.SetSoc(s.fetchedSoc)
}

// Restore seeds a fresh estimator from a persisted record.
//
// chargedEnergy is the loadpoint's current session counter, which the anchor
// is relative to — it is not necessarily zero, evcc may have restarted
// mid-session.
//
// Setting prevSoc to the anchor is essential: a fresh estimator has prevSoc 0,
// so the first poll would produce socDelta != 0, take the rebase branch and
// discard everything restored here.
//
// If neither the passed-in energyPerSocStep nor the estimator's own gradient
// is usable (both <= 0, e.g. a vehicle with 0 configured capacity), there is
// no Wh/% to convert energySinceAnchor into an offset. Dividing by that zero
// gradient would produce +/-Inf, which min(_, 100) silently clamps to a false
// 100% instead of erroring — so in that case vehicleSoc is seeded with the
// anchor alone, same as SetSoc refusing to touch a zero gradient.
func (s *Estimator) Restore(anchorSoc, energySinceAnchor, energyPerSocStep, chargedEnergy float64, learned bool) {
	if energyPerSocStep > 0 {
		s.energyPerSocStep = energyPerSocStep
		s.virtualCapacity = max(s.vehicle.Capacity()*1e3, energyPerSocStep*100)
		s.learned = learned
	}

	s.prevSoc = anchorSoc
	s.fetchedSoc = anchorSoc
	s.chargedEnergy = max(chargedEnergy, 0)
	s.prevChargedEnergy = s.chargedEnergy - energySinceAnchor

	if s.energyPerSocStep > 0 {
		s.vehicleSoc = min(anchorSoc+energySinceAnchor/s.energyPerSocStep, 100)
	} else {
		s.vehicleSoc = anchorSoc
	}
}
