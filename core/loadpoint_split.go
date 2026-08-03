package core

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/core/wrapper"
)

// SplitSession ends the running charging session and starts a new one without
// requiring the vehicle to be disconnected. It knows three variants: keep the
// current vehicle (v nil, detach false), assign v to the new session, or detach
// the vehicle altogether (detach true) for a guest car.
//
// The split is queued as a task so that it runs in the loadpoint's goroutine:
// lp.session, lp.energyMetrics, lp.chargeRater and the active vehicle are only
// ever touched from there, and the whole split stays one indivisible step
// instead of racing a concurrent vehicle change from the api goroutine.
//
// Note that this does not make the api call itself race-free: the task queue
// (util.Queue) carries no mutex, so enqueueing here and dequeueing in the
// loadpoint goroutine is unsynchronised. That is pre-existing upstream
// behaviour shared by every addTask caller and not fixed here.
func (lp *Loadpoint) SplitSession(v api.Vehicle, detach bool) {
	// enqueueTask, not addTask: addTask deduplicates by code pointer, which is
	// identical for all closures of this literal and would silently drop a
	// second split aimed at a different vehicle
	lp.enqueueTask(func() {
		lp.splitSession(v, detach)
	})
	lp.requestUpdate()
}

// splitSession finishes the current session and starts a new one, optionally
// assigning a different vehicle or detaching the current one. Must run in the
// loadpoint's goroutine.
func (lp *Loadpoint) splitSession(v api.Vehicle, detach bool) {
	if lp.db == nil || lp.session == nil {
		return
	}

	charging := lp.charging()

	lp.stopSession()

	// tasks run before publishChargeProgress, so lp.energyMetrics still holds
	// the previous cycle's value while the charge meter is read fresh - re-read
	// the charger's own energy so that window belongs to the finished leg
	// instead of getting lost between both
	lp.finalizeSessionEnergy()

	lp.clearSession()

	// rebase the energy counter onto the current meter reading so that neither
	// leg loses or double-counts energy
	if cr, ok := lp.chargeRater.(wrapper.ChargeResetter); ok {
		cr.ResetCharge()
	} else {
		lp.log.WARN.Printf("session split: charge rater %T cannot be reset - the finished session's energy will be counted again in the new one", lp.chargeRater)
	}

	// the charger's own timer keeps counting from plug-in; compensate in
	// stopSession instead
	if ct, ok := lp.chargeTimer.(wrapper.ChargeResetter); ok {
		ct.ResetCharge()
		lp.chargeDurationOffset = 0
	} else {
		lp.chargeDurationOffset = lp.chargeDuration
	}

	// switch the vehicle before creating the new session: createSession copies
	// the currently active vehicle's title, and setActiveVehicle only writes
	// session.Vehicle when a vehicle is set - it does not clear it for nil.
	// Ordering is what guarantees a detached split leaves the new session
	// without a vehicle name.
	if v != nil || detach {
		lp.setActiveVehicle(v)
	}

	lp.createSession()

	// while the charger stays in status C no evChargeStart fires, so Created
	// would remain zero and stopSession would drop the second leg silently
	if charging {
		lp.updateSession(func(s *session.Session) {
			s.Created = lp.clock.Now()
			if soc := lp.vehicleSoc; soc > 0 && !lp.chargerHasFeature(api.Heating) {
				s.SocStart = &soc
			}
		})
	}

	lp.log.INFO.Printf("session split, vehicle: %s", lp.session.Vehicle)
}
