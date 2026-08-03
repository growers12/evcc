package core

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/core/wrapper"
	"github.com/evcc-io/evcc/tariff"
	"github.com/jinzhu/now"
)

func (lp *Loadpoint) chargeMeterTotal() float64 {
	m, ok := api.Cap[api.MeterEnergy](lp.chargeMeter)
	if !ok {
		return 0
	}

	f, err := m.TotalEnergy()
	if err != nil {
		if !errors.Is(err, api.ErrNotAvailable) {
			lp.log.ERROR.Printf("charge total import: %v", err)
		}
		return 0
	}

	lp.log.DEBUG.Printf("charge total import: %.3fkWh", f)

	return f
}

// createSession creates a charging session. The created timestamp is empty until set by evChargeStartHandler.
// The session is not persisted yet. That will only happen when stopSession is called.
func (lp *Loadpoint) createSession() {
	// test guard
	if lp.db == nil || lp.session != nil {
		return
	}

	lp.session = lp.db.New(lp.chargeMeterTotal())

	if v := lp.GetVehicle(); v != nil {
		lp.session.Vehicle = v.GetTitle()
	} else if lp.chargerHasFeature(api.IntegratedDevice) {
		lp.session.Vehicle = lp.GetTitle()
	}

	if c, ok := api.Cap[api.Identifier](lp.charger); ok {
		if id, err := c.Identify(); err == nil {
			lp.session.Identifier = id
		}
	}

	if lp.site != nil {
		lp.session.ReferencePricePerKWh = tariff.AverageRate(lp.site.GetTariff(api.TariffUsageGrid), 24*time.Hour)
		lp.session.ReferenceCo2PerKWh = tariff.AverageRate(lp.site.GetTariff(api.TariffUsageCo2), 24*time.Hour)
	}

	// energy
	lp.energyMetrics.Reset()

	// the soc estimate's energy anchor is relative to this counter, and this is
	// the only place in evcc that resets it. Whoever built the estimator did so
	// against the *previous* session's total (evVehicleConnectHandler runs
	// vehicleDefaultOrDetect before createSession, splitSession likewise
	// switches the vehicle first), so the anchor has to be re-applied here or
	// the first poll of the new session reads the raw source value again.
	lp.restoreSocEstimate()

	lp.energyMetrics.Publish("session", lp)
	lp.publish(keys.ChargedEnergy, lp.GetChargedEnergy())
}

// applyEnergyMetrics writes current energy metrics into the session and persists it.
func (lp *Loadpoint) applyEnergyMetrics(s *session.Session) {
	if meterStop := lp.chargeMeterTotal(); meterStop > 0 {
		s.MeterStop = &meterStop
	}

	if soc := lp.vehicleSoc; soc > 0 && !lp.chargerHasFeature(api.Heating) {
		s.SocEnd = &soc
	}

	s.SolarPercentage = new(lp.energyMetrics.SolarPercentage())
	s.Price = lp.energyMetrics.Price()
	s.PricePerKWh = lp.energyMetrics.PricePerKWh()
	s.Co2PerKWh = lp.energyMetrics.Co2PerKWh()
	s.ChargedEnergy = lp.energyMetrics.TotalWh() / 1e3

	lp.db.Persist(s)
}

// stopSession ends a charging session segment and persists the session.
func (lp *Loadpoint) stopSession() {
	s := lp.session

	// test guard
	if lp.db == nil || s == nil {
		return
	}

	// abort the session if charging has never started
	if s.Created.IsZero() {
		return
	}

	s.Finished = lp.clock.Now()

	// charger-provided charge timers are plain counter registers that can jump
	// backwards (wallbox reboot, modbus reconnect). An offset larger than the
	// current reading is stale and gets dropped rather than mirrored into a
	// plausible looking but wrong duration; a negative reading clamps to zero.
	duration := lp.chargeDuration
	if lp.chargeDurationOffset <= duration {
		duration -= lp.chargeDurationOffset
	}
	s.ChargeDuration = new(max(duration, 0))

	lp.applyEnergyMetrics(s)
}

type sessionOption func(*session.Session)

// updateSession updates any parameter of a charging session and persists the session.
func (lp *Loadpoint) updateSession(opts ...sessionOption) {
	// test guard
	if lp.db == nil || lp.session == nil {
		return
	}

	for _, opt := range opts {
		opt(lp.session)
	}

	if !lp.session.Created.IsZero() {
		lp.db.Persist(lp.session)
	}
}

// clearSession clears the charging session without persisting it.
func (lp *Loadpoint) clearSession() {
	// test guard
	if lp.db == nil {
		return
	}

	lp.session = nil
}

func (lp *Loadpoint) finalizeSessionEnergy() {
	s := lp.session
	if lp.db == nil || s == nil || s.Created.IsZero() {
		return
	}

	f, err := lp.chargeRater.ChargedEnergy()
	if err != nil {
		lp.log.ERROR.Printf("session energy: %v", err)
		return
	}

	chargedKWh := f - lp.chargedAtStartup
	if chargedKWh <= s.ChargedEnergy {
		return
	}

	lp.log.DEBUG.Printf("session energy: %.3f -> %.3fkWh", s.ChargedEnergy, chargedKWh)

	lp.energyMetrics.Update(chargedKWh)

	lp.applyEnergyMetrics(s)
}

func (lp *Loadpoint) resetHeatingSession() {
	if lp.session == nil || !lp.chargerHasFeature(api.Heating) || !lp.chargerHasFeature(api.IntegratedDevice) {
		return
	}

	if !now.With(lp.clock.Now()).BeginningOfDay().After(lp.session.Created) {
		return
	}

	lp.stopSession()
	lp.clearSession()

	if cr, ok := lp.chargeRater.(wrapper.ChargeResetter); ok {
		cr.ResetCharge()
	}
	if ct, ok := lp.chargeTimer.(wrapper.ChargeResetter); ok {
		ct.ResetCharge()
	}

	lp.createSession()
}

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
