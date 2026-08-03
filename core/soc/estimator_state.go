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
func (s *Estimator) SetSoc(target float64) error {
	if target < 0 || target > 100 {
		return fmt.Errorf("soc out of range: %.1f", target)
	}

	if s.energyPerSocStep <= 0 {
		return errors.New("no gradient available")
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

// Restore seeds a fresh estimator from a persisted record.
//
// chargedEnergy is the loadpoint's current session counter, which the anchor
// is relative to — it is not necessarily zero, evcc may have restarted
// mid-session.
//
// Setting prevSoc to the anchor is essential: a fresh estimator has prevSoc 0,
// so the first poll would produce socDelta != 0, take the rebase branch and
// discard everything restored here.
func (s *Estimator) Restore(anchorSoc, energySinceAnchor, energyPerSocStep, chargedEnergy float64, learned bool) {
	if energyPerSocStep > 0 {
		s.energyPerSocStep = energyPerSocStep
		s.virtualCapacity = max(s.vehicle.Capacity()*1e3, energyPerSocStep*100)
		s.learned = learned
	}

	s.prevSoc = anchorSoc
	s.fetchedSoc = anchorSoc
	s.prevChargedEnergy = chargedEnergy - energySinceAnchor
	s.vehicleSoc = min(anchorSoc+energySinceAnchor/s.energyPerSocStep, 100)
}
