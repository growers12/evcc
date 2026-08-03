package core

import (
	"fmt"
	"time"

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
// The estimator's own prevChargedEnergy is relative to the session counter and
// therefore useless across an unplug. energySinceAnchor is accumulated here
// instead: the session's contribution is folded into a persisted base, so the
// figure survives unplugging, session splits and restarts.
func (lp *Loadpoint) updateSocEstimate() {
	if lp.socEstimator == nil || lp.socEstimateVehicle == "" {
		return
	}

	st := lp.socEstimator.State()
	if st.EnergyPerSocStep <= 0 {
		return
	}

	se, _ := loadSocEstimate(lp.socEstimateVehicle)

	// the estimator rebases prevSoc whenever the source reports a changed
	// value; that is the moment the blind phase ends
	if se.AnchorSoc != st.PrevSoc {
		se.AnchorSoc = st.PrevSoc
		se.EnergySinceAnchor = 0
		se.OdometerAtAnchor = lp.socEstimateOdometer
		lp.socEstimateBase = 0
	}

	// energy the estimator currently attributes to this anchor
	se.EnergySinceAnchor = lp.socEstimateBase + (st.VehicleSoc-st.PrevSoc)*st.EnergyPerSocStep
	se.EnergyPerSocStep = st.EnergyPerSocStep
	se.Updated = lp.clock.Now()

	if err := saveSocEstimate(lp.socEstimateVehicle, se); err != nil {
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
		lp.socEstimateBase = 0
		return
	}

	lp.socEstimator.Restore(se.AnchorSoc, se.EnergySinceAnchor, se.EnergyPerSocStep, lp.GetChargedEnergy(), se.Samples > 0)

	// Restore already folds energySinceAnchor into the estimator's own
	// prevChargedEnergy (see its doc comment), so the freshly restored
	// estimator's (VehicleSoc-PrevSoc)*EnergyPerSocStep already equals the
	// full total since the anchor. socEstimateBase must therefore start at
	// zero here — adding se.EnergySinceAnchor again in updateSocEstimate
	// would double-count everything the estimator already carries forward.
	lp.socEstimateBase = 0

	lp.log.INFO.Printf("soc estimate restored: %.1f%% (anchor %.1f%%, %.0f Wh since)", se.soc(), se.AnchorSoc, se.EnergySinceAnchor)
}
