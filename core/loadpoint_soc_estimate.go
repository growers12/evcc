package core

import (
	"errors"
	"fmt"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/server/db/settings"
)

const (
	// socEstimateKey is the per-vehicle settings key, matching the
	// vehicle.<name>.<key> scheme evcc already uses for minSoc and limitSoc
	socEstimateKey = "socEstimate"

	// socEstimateMaxAge is how long a record may go unattended before its
	// offset is dropped. It is measured against Updated, the write timestamp,
	// because that is what an unattended record means here: while evcc runs,
	// every poll re-derives EnergySinceAnchor from the charger's own counter,
	// so the offset cannot drift. Only evcc not running can make it wrong -
	// the car may have been driven and charged elsewhere meanwhile.
	//
	// Explicitly *not* measured against AnchorUpdated: a car parked in a
	// garage without network is expected to sit on the same anchor for days,
	// which is the entire reason this feature exists. Discarding its offset on
	// a restart would recreate the incident the feature prevents.
	socEstimateMaxAge = 24 * time.Hour

	// socEstimateMaxOffset caps a restored offset, in percentage points
	socEstimateMaxOffset = 50.0

	// socLearnMinRise is the smallest soc rise worth learning from, in points
	socLearnMinRise = 10.0

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
	AnchorUpdated     time.Time `json:"anchorUpdated"`     // when the vehicle last reported a changed soc
	Updated           time.Time `json:"updated"`           // when this record was last written
}

// soc returns the estimate this record represents, clamped to 100% for display
func (se SocEstimate) soc() float64 {
	if se.EnergyPerSocStep <= 0 {
		return se.AnchorSoc
	}
	return min(se.AnchorSoc+se.EnergySinceAnchor/se.EnergyPerSocStep, 100)
}

// offset returns the estimate's distance above the anchor, in percentage
// points. Deliberately derived from the energy rather than from soc(): the
// clamp in there would report an anchor of 60 with a 200 point offset as a
// harmless 40 and let it pass the plausibility check.
func (se SocEstimate) offset() float64 {
	if se.EnergyPerSocStep <= 0 {
		return 0
	}
	return se.EnergySinceAnchor / se.EnergyPerSocStep
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

// learnable reports whether a re-anchor onto newSoc could produce a usable
// gradient measurement at all.
func (se SocEstimate) learnable(newSoc float64) bool {
	return newSoc-se.AnchorSoc > socLearnMinRise && se.EnergySinceAnchor > 0
}

// learn folds a fresh vehicle reading into the gradient and re-anchors the
// record. The bool reports whether the gradient was actually updated.
//
// There is deliberately no odometer guard. It was tried and removed: this
// vehicle only writes its odometer at ignition-off, which happens in the
// garage without reception, so the value that arrives together with the fresh
// soc describes the drive *before* the anchor - energy that was consumed
// before the anchor soc was measured and cannot corrupt this measurement. The
// guard therefore rejected every real learning opportunity while protecting
// against nothing. The case it was meant to catch - a long drive between
// unplug and the fresh reading - is small in practice, because the car reports
// within a few hundred metres of leaving the garage.
//
// What remains as protection: a rise of more than socLearnMinRise excludes
// noise, the sanity band excludes meter glitches and vehicle mix-ups, and the
// EMA lets a single bad sample move the gradient by at most 30% of its error.
//
// The anchor is re-set even when learning is rejected. Otherwise a single
// missed learning moment would leave EnergySinceAnchor growing against a stale
// anchor forever.
func (se SocEstimate) learn(newSoc float64, capacity float64) (SocEstimate, bool) {
	rise := newSoc - se.AnchorSoc
	nominal := capacity * 1e3 / soc.ChargeEfficiency / 100

	learned := false

	if se.learnable(newSoc) {
		// energySinceAnchor is measured at the charger, so the learned value
		// carries the real charging losses — which is exactly what the 85%
		// efficiency factor stands in for until then. Both of the factors that
		// go into that default are optimistic: it uses gross rather than
		// usable capacity, and 85% rather than the ~90%+ a three-phase 11 kW
		// charge actually reaches. Measured values therefore land some 15%
		// below it, which is the correction this is here to make.
		measured := se.EnergySinceAnchor / rise

		if measured >= 0.5*nominal && measured <= 2*nominal {
			se.EnergyPerSocStep = (1-socLearnSmoothing)*se.EnergyPerSocStep + socLearnSmoothing*measured
			se.Samples++
			learned = true
		}
	}

	se.AnchorSoc = newSoc
	se.EnergySinceAnchor = 0

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

// LoadSocEstimate reads a vehicle's persisted estimate. Exported for the api
// and the site's vehicle publisher, which need it for vehicles that are not
// currently connected to any loadpoint.
func LoadSocEstimate(name string) (SocEstimate, bool) {
	return loadSocEstimate(name)
}

// Soc returns the estimate this record represents
func (se SocEstimate) Soc() float64 {
	return se.soc()
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
// estimator, vehicleName and v are the caller's snapshot of lp.socEstimator,
// lp.socEstimateVehicle and the active vehicle. They are passed in rather than
// read from lp because setActiveVehicle runs synchronously on the http and
// mqtt goroutines: re-reading any of them here could pair car A's estimator
// with car B's name and write A's energy into B's record - both records
// staying syntactically valid, which is why the tearing would go unnoticed.
// The same reason upstream snapshots socEstimator in publishSocAndRange, see
// evcc issue 16180.
//
// There is no separate cross-session bookkeeping here: the estimator is
// rebuilt from scratch on every vehicle connect (setActiveVehicle), and
// restoreSocEstimate's call to Restore() folds the persisted energySinceAnchor
// back into the fresh estimator's prevChargedEnergy. That means the running
// estimator's own energy counters already carry the full history since the
// anchor — this function just reads them back out.
func (lp *Loadpoint) updateSocEstimate(estimator *soc.Estimator, vehicleName string, v api.Vehicle) {
	if estimator == nil || vehicleName == "" {
		return
	}

	st := estimator.State()
	if st.EnergyPerSocStep <= 0 {
		return
	}

	var capacity float64
	if v != nil {
		capacity = v.Capacity()
	}

	se, _ := loadSocEstimate(vehicleName)

	// the estimator rebases prevSoc whenever the source reports a changed
	// value; that is the moment the blind phase ends and learning is possible
	learnedThisCall := false
	if se.AnchorSoc != st.PrevSoc {
		se, learnedThisCall = se.learn(st.PrevSoc, capacity)
		se.AnchorUpdated = lp.clock.Now()

		if learnedThisCall {
			lp.log.INFO.Printf("soc gradient learned: %.1f Wh/%% (%d samples)", se.EnergyPerSocStep, se.Samples)

			// push the learned gradient into the live estimator, not just the
			// persisted record — otherwise it only survives until the next
			// poll, when st.EnergyPerSocStep (read from the estimator, which
			// nobody but Restore ever updates) is mirrored back below and
			// silently reverts the just-learned value.
			//
			// Restore changes energyPerSocStep, virtualCapacity and learned
			// here and nothing else: the estimator just rebased onto exactly
			// this anchor, so prevSoc, fetchedSoc and vehicleSoc already equal
			// se.AnchorSoc and prevChargedEnergy already equals
			// st.PrevChargedEnergy. Passing st.PrevChargedEnergy rather than
			// lp.GetChargedEnergy() keeps that exact by construction instead
			// of by assuming the counter has not moved since the poll.
			estimator.Restore(se.AnchorSoc, 0, se.EnergyPerSocStep, st.PrevChargedEnergy, true)
		}
	}

	// energy the estimator currently attributes to this anchor. Taken from the
	// energy counters, not from (VehicleSoc-PrevSoc)*EnergyPerSocStep: once the
	// estimate saturates at the 100% clamp in soc.Estimator.Soc, the soc
	// difference stops growing and every further kWh would be dropped from the
	// record - and with it from the next learn's input.
	se.EnergySinceAnchor = st.ChargedEnergy - st.PrevChargedEnergy

	// when this call did not learn, mirror the live estimator's own gradient
	// (e.g. its upstream in-session learner, see soc.Estimator.Soc). A gradient
	// learned above must survive this line — st was captured before the
	// Restore() push above and still holds the pre-learn value; on later calls
	// st and se already agree, so this line is a no-op.
	if !learnedThisCall {
		se.EnergyPerSocStep = st.EnergyPerSocStep
	}
	se.Updated = lp.clock.Now()

	if err := saveSocEstimate(vehicleName, se); err != nil {
		lp.log.ERROR.Printf("soc estimate: %v", err)
	}
}

// restoreSocEstimate seeds an estimator from the persisted record.
//
// Called from two places, and it has to be both: setActiveVehicle right after
// NewEstimator, because a mid-session vehicle change gets no new session; and
// createSession right after energyMetrics.Reset(), because the anchor is
// relative to that counter and setActiveVehicle always runs before the reset.
// On the connect and split paths it therefore runs twice, the second call
// winning - Restore only ever reads the record and writes estimator fields, so
// repeating it is harmless.
//
// Deliberately *not* solved by making Restore defer the anchor to the first
// Soc() call: between a mid-session vehicle change and the next poll real
// energy flows, and a deferred anchor would silently swallow it.
func (lp *Loadpoint) restoreSocEstimate() {
	if lp.socEstimator == nil || lp.socEstimateVehicle == "" {
		return
	}

	se, ok := loadSocEstimate(lp.socEstimateVehicle)
	if !ok {
		return
	}

	// a *learned* gradient is a property of the car and always worth keeping.
	// An unlearned one is just the static default the record happened to copy
	// at the time, and restoring it would pin the estimator to a capacity that
	// may since have been corrected in the config - passing 0 leaves the fresh
	// estimator's own default in place.
	gradient := 0.0
	if se.Samples > 0 {
		gradient = se.EnergyPerSocStep
	}

	// the offset is restored only if the record is still plausible. Should the
	// source have moved on while evcc was down, the estimator's rebase branch
	// drops the offset on the first poll — see the comment on plausible().
	if !se.plausible(lp.clock.Now()) {
		lp.log.DEBUG.Printf("soc estimate: offset discarded, keeping gradient %.1f Wh/%%", se.EnergyPerSocStep)
		lp.socEstimator.Restore(se.AnchorSoc, 0, gradient, lp.GetChargedEnergy(), se.Samples > 0)
		return
	}

	lp.socEstimator.Restore(se.AnchorSoc, se.EnergySinceAnchor, gradient, lp.GetChargedEnergy(), se.Samples > 0)

	lp.log.INFO.Printf("soc estimate restored: %.1f%% (anchor %.1f%%, %.0f Wh since)", se.soc(), se.AnchorSoc, se.EnergySinceAnchor)

	// not a reason to discard anything - a car parked without network keeps its
	// anchor for days by design - but worth saying out loud, because the older
	// the anchor the more of the estimate rests on the gradient alone
	if age := lp.clock.Now().Sub(se.AnchorUpdated); age > socEstimateMaxAge && !se.AnchorUpdated.IsZero() {
		lp.log.WARN.Printf("soc estimate: anchor is %.0fh old, the vehicle has not reported a soc since", age.Hours())
	}
}

// ErrNoSocEstimator is returned when the loadpoint has no active estimator
var ErrNoSocEstimator = errors.New("no soc estimator active")

// GetSocEstimate returns the live estimator state and the persisted record
func (lp *Loadpoint) GetSocEstimate() (soc.State, SocEstimate, bool) {
	lp.RLock()
	defer lp.RUnlock()

	if lp.socEstimator == nil {
		return soc.State{}, SocEstimate{}, false
	}

	se, _ := loadSocEstimate(lp.socEstimateVehicle)

	return lp.socEstimator.State(), se, true
}

// socEstimateTarget snapshots the estimator together with everything
// updateSocEstimate needs to attribute its result to the right car, and
// validates target against the estimator's current state.
//
// The snapshot is taken on the caller's goroutine and the queued closure
// closes over it instead of re-reading lp at task execution time. Between
// enqueueing and draining, setActiveVehicle can set lp.socEstimator to nil or
// to a different instance (reachable from the HTTP and MQTT vehicle-select
// paths, not only through the task queue) - re-reading the field inside the
// closure would risk a nil dereference in processTasks, which has no
// recover() and would crash the whole process.
//
// Validation happens here rather than in the task because the task runs after
// the HTTP response has been sent: an error raised in there would be a log
// line under a 200.
func (lp *Loadpoint) socEstimateTarget(target float64) (*soc.Estimator, string, api.Vehicle, error) {
	estimator := lp.socEstimator
	if estimator == nil {
		return nil, "", nil, ErrNoSocEstimator
	}

	vehicleName := lp.socEstimateVehicle
	v := lp.GetVehicle()

	if target < 0 || target > 100 {
		return nil, "", nil, fmt.Errorf("soc out of range: %.1f", target)
	}

	if st := estimator.State(); target < st.FetchedSoc {
		return nil, "", nil, fmt.Errorf("soc estimate cannot be set below the value reported by the vehicle (%.1f%%): %.1f", st.FetchedSoc, target)
	}

	return estimator, vehicleName, v, nil
}

// SetSocEstimate overrides the estimated soc. The actual write runs inside the
// loadpoint task queue so the estimator is never touched from the HTTP
// goroutine; see socEstimateTarget for the snapshot and validation rules.
func (lp *Loadpoint) SetSocEstimate(v float64) error {
	estimator, vehicleName, vehicle, err := lp.socEstimateTarget(v)
	if err != nil {
		return err
	}

	lp.enqueueTask(func() {
		// defensive: estimator is a snapshot taken before enqueueing and is
		// never nil here, but check anyway - consistent with the nil check
		// updateSocEstimate itself already performs on its estimator param.
		if estimator == nil {
			return
		}

		if err := estimator.SetSoc(v); err != nil {
			lp.log.ERROR.Printf("soc estimate: %v", err)
			return
		}

		lp.updateSocEstimate(estimator, vehicleName, vehicle)
		lp.log.INFO.Printf("soc estimate set to %.1f%%", v)
	})
	lp.requestUpdate()

	return nil
}

// ShiftSocEstimate moves the energy anchor by kwh. See the socEstimateTarget
// doc comment for why the estimator is snapshotted before enqueueing rather
// than re-read from lp.socEstimator inside the closure.
func (lp *Loadpoint) ShiftSocEstimate(kwh float64) error {
	estimator := lp.socEstimator
	if estimator == nil {
		return ErrNoSocEstimator
	}

	st := estimator.State()
	if st.EnergyPerSocStep <= 0 {
		return errors.New("no gradient available")
	}

	// validate the resulting soc, not the shift: a negative shift below the
	// source value is the operator's obvious move after over-booking energy,
	// and would otherwise answer 200 and revert on the next poll
	estimator, vehicleName, vehicle, err := lp.socEstimateTarget(st.VehicleSoc + kwh*1e3/st.EnergyPerSocStep)
	if err != nil {
		return err
	}

	lp.enqueueTask(func() {
		if estimator == nil {
			return
		}

		if err := estimator.ShiftEnergy(kwh * 1e3); err != nil {
			lp.log.ERROR.Printf("soc estimate: %v", err)
			return
		}

		lp.updateSocEstimate(estimator, vehicleName, vehicle)
		lp.log.INFO.Printf("soc estimate shifted by %.2f kWh", kwh)
	})
	lp.requestUpdate()

	return nil
}

// ClearSocEstimate drops the override and the persisted record, so the
// estimate follows the vehicle's own value again.
//
// Deleting the record alone is a no-op the operator cannot see: the offset
// lives in the running estimator's prevChargedEnergy, and the very next poll
// re-anchors and writes an identical record back. The estimator therefore has
// to be reset first, in the same task, so no poll can slip in between.
func (lp *Loadpoint) ClearSocEstimate() error {
	name := lp.socEstimateVehicle
	if name == "" {
		return ErrNoSocEstimator
	}

	estimator := lp.socEstimator

	lp.enqueueTask(func() {
		if estimator != nil {
			if err := estimator.ResetOverride(); err != nil {
				lp.log.ERROR.Printf("soc estimate: %v", err)
			}
		}

		if err := deleteSocEstimate(name); err != nil {
			lp.log.ERROR.Printf("soc estimate: %v", err)
			return
		}

		lp.log.INFO.Printf("soc estimate cleared, following the vehicle again")
	})
	lp.requestUpdate()

	return nil
}
