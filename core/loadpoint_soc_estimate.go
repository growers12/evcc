package core

import (
	"fmt"
	"time"

	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/server/db/settings"
)

const (
	// socEstimateKey is the per-vehicle settings key, matching the
	// vehicle.<name>.<key> scheme evcc already uses for minSoc and limitSoc
	socEstimateKey = "socEstimate"

	// socEstimateMaxAge discards a record whose anchor is too old to trust
	socEstimateMaxAge = 24 * time.Hour

	// socEstimateMaxOffset caps a restored offset, in percentage points
	socEstimateMaxOffset = 50.0

	// socLearnMinRise is the smallest soc rise worth learning from, in points
	socLearnMinRise = 10.0

	// socLearnMaxDistance caps the driving between unplug and the first fresh
	// reading, in km. The car only regains network after leaving the garage,
	// so some driving is unavoidable — but not an arbitrary amount.
	socLearnMaxDistance = 20.0

	// socLearnSmoothing weights a new measurement against the running value
	socLearnSmoothing = 0.3
)

// SocEstimate is the persisted state of a vehicle's soc estimation.
//
// It is kept per vehicle rather than per loadpoint because two cars share one
// loadpoint here, and it accumulates EnergySinceAnchor itself rather than
// deriving it from the session counter — the counter resets on unplug, which
// is exactly the moment before the value is needed.
type SocEstimate struct {
	AnchorSoc         float64   `json:"anchorSoc"`         // last soc the vehicle actually reported, in %
	EnergySinceAnchor float64   `json:"energySinceAnchor"` // energy delivered at the charger since, in Wh
	EnergyPerSocStep  float64   `json:"energyPerSocStep"`  // Wh per soc percentage point
	Samples           int       `json:"samples"`           // number of completed learning cycles
	OdometerAtAnchor  float64   `json:"odometerAtAnchor"`  // km reading when the anchor was set
	Updated           time.Time `json:"updated"`
}

// soc returns the estimate this record represents
func (se SocEstimate) soc() float64 {
	if se.EnergyPerSocStep <= 0 {
		return se.AnchorSoc
	}
	return min(se.AnchorSoc+se.EnergySinceAnchor/se.EnergyPerSocStep, 100)
}

// offset returns the estimate's distance above the anchor, in percentage points
func (se SocEstimate) offset() float64 {
	return se.soc() - se.AnchorSoc
}

// plausible reports whether the record's offset may be restored. The gradient
// is restored regardless — it is a property of the car, not of the session.
//
// There is deliberately no check against the current source value here. At
// restore time no value has been fetched yet, and none is needed: Restore sets
// prevSoc to the anchor, so if the first poll delivers a different value the
// estimator's own rebase branch discards the offset. The stale-source guard
// from the spec is that behaviour, not a condition in this function.
func (se SocEstimate) plausible(now time.Time) bool {
	switch {
	case se.EnergyPerSocStep <= 0:
		return false
	case now.Sub(se.Updated) > socEstimateMaxAge:
		return false
	case se.offset() > socEstimateMaxOffset:
		return false
	default:
		return true
	}
}

// learn folds a fresh vehicle reading into the gradient and re-anchors the
// record. The bool reports whether the gradient was actually updated.
//
// The anchor is re-set even when learning is rejected. Otherwise a single
// missed learning moment — a long drive before the car reports — would leave
// EnergySinceAnchor growing against a stale anchor forever.
func (se SocEstimate) learn(newSoc, odometer, capacity float64) (SocEstimate, bool) {
	rise := newSoc - se.AnchorSoc
	distance := odometer - se.OdometerAtAnchor
	nominal := capacity * 1e3 / soc.ChargeEfficiency / 100

	learned := false

	switch {
	case rise < socLearnMinRise:
	case se.EnergySinceAnchor <= 0:
	case odometer > 0 && se.OdometerAtAnchor > 0 && distance > socLearnMaxDistance:
	default:
		// energySinceAnchor is measured at the charger, so the learned value
		// carries the real charging losses — which is exactly what the 85%
		// efficiency factor stands in for until then
		measured := se.EnergySinceAnchor / rise

		if measured >= 0.5*nominal && measured <= 2*nominal {
			se.EnergyPerSocStep = (1-socLearnSmoothing)*se.EnergyPerSocStep + socLearnSmoothing*measured
			se.Samples++
			learned = true
		}
	}

	se.AnchorSoc = newSoc
	se.EnergySinceAnchor = 0
	se.OdometerAtAnchor = odometer

	return se, learned
}

func socEstimateSettingsKey(name string) string {
	return fmt.Sprintf("vehicle.%s.%s", name, socEstimateKey)
}

// loadSocEstimate reads a vehicle's persisted estimate
func loadSocEstimate(name string) (SocEstimate, bool) {
	var se SocEstimate
	if err := settings.Json(socEstimateSettingsKey(name), &se); err != nil {
		return SocEstimate{}, false
	}
	return se, true
}

// saveSocEstimate writes a vehicle's estimate. The value only reaches SQLite
// when evcc's own one-minute settings ticker flushes it.
func saveSocEstimate(name string, se SocEstimate) error {
	return settings.SetJson(socEstimateSettingsKey(name), se)
}

// deleteSocEstimate removes a vehicle's persisted estimate
func deleteSocEstimate(name string) error {
	return settings.Delete(socEstimateSettingsKey(name))
}

// updateSocEstimate mirrors the running estimator into the persisted record.
//
// estimator is the caller's already-snapshotted copy of lp.socEstimator (see
// the race-condition guard in publishSocAndRange, evcc issue 16180) — reading
// lp.socEstimator or lp.socEstimateVehicle again here could tear an
// (estimator, vehicle name) pair apart if setActiveVehicle runs concurrently.
//
// There is no separate cross-session bookkeeping here: the estimator is
// rebuilt from scratch on every vehicle connect (setActiveVehicle), and
// restoreSocEstimate's call to Restore() folds the persisted energySinceAnchor
// back into the fresh estimator's prevChargedEnergy. That means the running
// estimator's own (VehicleSoc-PrevSoc)*EnergyPerSocStep already carries the
// full history since the anchor — this function just reads it back out.
func (lp *Loadpoint) updateSocEstimate(estimator *soc.Estimator) {
	vehicleName := lp.socEstimateVehicle
	if estimator == nil || vehicleName == "" {
		return
	}

	st := estimator.State()
	if st.EnergyPerSocStep <= 0 {
		return
	}

	se, _ := loadSocEstimate(vehicleName)

	// the estimator rebases prevSoc whenever the source reports a changed
	// value; that is the moment the blind phase ends and learning is possible
	learnedThisCall := false
	if se.AnchorSoc != st.PrevSoc {
		se, learnedThisCall = se.learn(st.PrevSoc, lp.socEstimateOdometer, lp.vehicleCapacity())

		if learnedThisCall {
			lp.log.INFO.Printf("soc gradient learned: %.1f Wh/%% (%d samples)", se.EnergyPerSocStep, se.Samples)

			// push the learned gradient into the live estimator, not just the
			// persisted record — otherwise it only survives until the next
			// poll, when st.EnergyPerSocStep (read from the estimator, which
			// nobody but Restore ever updates) is mirrored back below and
			// silently reverts the just-learned value. Restore is idempotent
			// here: the estimator just rebased onto exactly this anchor and
			// energy, so every field it writes except the gradient itself is
			// a no-op — see prevSoc/fetchedSoc/vehicleSoc/prevChargedEnergy
			// against soc.Estimator.Soc's rebase branch above.
			estimator.Restore(se.AnchorSoc, 0, se.EnergyPerSocStep, lp.GetChargedEnergy(), true)
		}
	}

	// energy the estimator currently attributes to this anchor
	se.EnergySinceAnchor = (st.VehicleSoc - st.PrevSoc) * st.EnergyPerSocStep

	// outside a learning call this call, mirror the live estimator's own
	// gradient (e.g. its upstream in-session learner, see soc.Estimator.Soc).
	// A gradient learned above this call must survive this line — st was
	// captured before the Restore() push above and still holds the pre-learn
	// value; on later calls st and se already agree, so this line is a no-op.
	if !learnedThisCall {
		se.EnergyPerSocStep = st.EnergyPerSocStep
	}
	se.Updated = lp.clock.Now()

	if err := saveSocEstimate(vehicleName, se); err != nil {
		lp.log.ERROR.Printf("soc estimate: %v", err)
	}
}

// restoreSocEstimate seeds a freshly created estimator from the persisted
// record. Called right after NewEstimator, i.e. on every vehicle connect.
func (lp *Loadpoint) restoreSocEstimate() {
	if lp.socEstimator == nil || lp.socEstimateVehicle == "" {
		return
	}

	se, ok := loadSocEstimate(lp.socEstimateVehicle)
	if !ok {
		return
	}

	// the gradient is a property of the car and always worth keeping; the
	// offset only if the record is still plausible. Should the source have
	// moved on while evcc was down, the estimator's rebase branch drops the
	// offset on the first poll — see the comment on plausible().
	if !se.plausible(lp.clock.Now()) {
		lp.log.DEBUG.Printf("soc estimate: offset discarded, keeping gradient %.1f Wh/%%", se.EnergyPerSocStep)
		lp.socEstimator.Restore(se.AnchorSoc, 0, se.EnergyPerSocStep, lp.GetChargedEnergy(), se.Samples > 0)
		return
	}

	lp.socEstimator.Restore(se.AnchorSoc, se.EnergySinceAnchor, se.EnergyPerSocStep, lp.GetChargedEnergy(), se.Samples > 0)

	lp.log.INFO.Printf("soc estimate restored: %.1f%% (anchor %.1f%%, %.0f Wh since)", se.soc(), se.AnchorSoc, se.EnergySinceAnchor)
}

// vehicleCapacity returns the active vehicle's nominal capacity in kWh
func (lp *Loadpoint) vehicleCapacity() float64 {
	if v := lp.GetVehicle(); v != nil {
		return v.Capacity()
	}
	return 0
}
