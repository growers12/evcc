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
