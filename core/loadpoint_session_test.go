package core

import (
	"bytes"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/core/settings"
	"github.com/evcc-io/evcc/core/wrapper"
	serverdb "github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func sessionStart(lp *Loadpoint) func(session *session.Session) {
	return func(session *session.Session) {
		if session.Created.IsZero() {
			session.Created = lp.clock.Now()
		}
	}
}

func TestSession(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}

	cm := &EnergyDecorator{Meter: mm, MeterEnergy: me}

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock,
		db:          db,
		chargeMeter: cm,
	}

	// create session
	me.EXPECT().TotalEnergy().Return(1.0, nil)
	lp.createSession()
	assert.NotNil(t, lp.session)

	// start charging
	lp.updateSession(sessionStart(lp))
	assert.Equal(t, clock.Now(), lp.session.Created)

	// stop charging
	clock.Add(time.Hour)
	lp.energyMetrics.Update(1.23)
	me.EXPECT().TotalEnergy().Return(1.0+lp.getChargedEnergy()/1e3, nil) // match chargedEnergy

	lp.stopSession()
	assert.NotNil(t, lp.session)
	assert.Equal(t, lp.getChargedEnergy()/1e3, lp.session.ChargedEnergy)
	assert.Equal(t, clock.Now(), lp.session.Finished)

	s, err := db.Sessions()
	require.NoError(t, err)
	assert.Len(t, s, 1)
	t.Logf("session: %+v", s)

	// stop charging - 2nd leg
	clock.Add(time.Hour)
	lp.energyMetrics.Update(lp.getChargedEnergy() * 2)
	me.EXPECT().TotalEnergy().Return(3.0, nil) // doesn't match chargedEnergy

	lp.stopSession()
	assert.NotNil(t, lp.session)
	assert.Equal(t, clock.Now(), lp.session.Finished)

	s, err = db.Sessions()
	require.NoError(t, err)
	assert.Len(t, s, 1)
	t.Logf("session: %+v", s)
}

func TestCloseSessionsOnStartup_emptyDb(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	// assert empty DB is no problem
	err = db.ClosePendingSessionsInHistory(1000)
	require.NoError(t, err)
}

func TestCloseSessionsOnStartup(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db1, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	db2, err := session.NewStore("bar", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	// test data, creates 6 sessions for each loadpoint, 3rd and 6th are "unfinished"
	sessions1 := createMockSessions(db1, clock)
	sessions2 := createMockSessions(db2, clock)

	// write interleaved for two loadpoints
	for index, session := range sessions1 {
		db1.Persist(session)
		db2.Persist(sessions2[index])
	}

	err = db1.ClosePendingSessionsInHistory(1000)
	require.NoError(t, err)

	// check fixed sessions for db1
	var db1Sessions session.Sessions
	err = serverdb.Instance.Where("Loadpoint = ?", "foo").Order("ID").Find(&db1Sessions).Error
	require.NoError(t, err)
	assert.Len(t, db1Sessions, 6)

	// check fixed history
	for _, s := range db1Sessions[:5] {
		assert.NotEmpty(t, s.MeterStop)
		assert.Equal(t, float64(10), s.ChargedEnergy)
		t.Logf("session: %+v", s)
	}

	// check fixed most recent record
	assert.NotEmpty(t, db1Sessions[5].MeterStop)
	assert.Equal(t, float64(940), db1Sessions[5].ChargedEnergy)

	// ensure no side effects on loadpoint 2 data, i.e. data left unfixed
	var db2Sessions session.Sessions
	err = serverdb.Instance.Where("Loadpoint = ?", "bar").Order("ID").Find(&db2Sessions).Error
	require.NoError(t, err)
	assert.Len(t, db2Sessions, 6)

	for i, s := range db2Sessions {
		if (i+1)%3 == 0 {
			assert.Empty(t, s.MeterStop)
			assert.Empty(t, s.ChargedEnergy)
			continue
		}
		assert.NotEmpty(t, s.MeterStop)
		assert.Equal(t, float64(10), s.ChargedEnergy)
	}
}

func createMockSessions(db *session.DB, clock *clock.Mock) []*session.Session {
	var sessions []*session.Session
	for i := 1; i <= 6; i++ {
		meter1Start := float64(i * 10)
		session := db.New(meter1Start)
		session.Created = clock.Now().Add(1 * time.Minute)

		// create every third session as incomplete
		if i%3 == 0 {
			sessions = append(sessions, session)
			continue
		}

		session.Finished = clock.Now().Add(2 * time.Minute)
		meterStop := meter1Start + 10
		session.MeterStop = &meterStop
		session.ChargedEnergy = 10
		sessions = append(sessions, session)
	}
	return sessions
}

func TestResetHeatingSession(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cc := api.NewMockCharger(ctrl)
	fd := api.NewMockFeatureDescriber(ctrl)

	type FeatureDecorator struct {
		api.Charger
		api.FeatureDescriber
	}

	charger := &FeatureDecorator{Charger: cc, FeatureDescriber: fd}
	fd.EXPECT().Features().AnyTimes().Return([]api.Feature{
		api.Heating, api.IntegratedDevice,
	})

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}

	cm := &EnergyDecorator{Meter: mm, MeterEnergy: me}

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock,
		db:          db,
		charger:     charger,
		chargeMeter: cm,
	}

	// create session
	me.EXPECT().TotalEnergy().Return(1.0, nil)
	lp.createSession()
	require.NotNil(t, lp.session)
	assert.True(t, lp.session.Created.IsZero())

	// actually mark session as started
	lp.session.Created = clock.Now()
	assert.Equal(t, clock.Now(), lp.session.Created)

	clock.Add(36 * time.Hour)
	me.EXPECT().TotalEnergy().Return(1.0, nil).MaxTimes(2)

	lp.resetHeatingSession()
	require.NotNil(t, lp.session)
	assert.True(t, lp.session.Created.IsZero())

	lp.updateSession(sessionStart(lp))
	assert.Equal(t, clock.Now(), lp.session.Created)

	me.EXPECT().TotalEnergy().Return(3.0, nil)
	lp.stopSession()

	assert.NotNil(t, lp.session)
	assert.Equal(t, clock.Now(), lp.session.Finished)
	assert.Equal(t, 1.0, *lp.session.MeterStart)
	assert.Equal(t, 3.0, *lp.session.MeterStop)
}

func TestFinalizeSessionEnergy(t *testing.T) {
	setup := func(t *testing.T) (*Loadpoint, *api.MockMeterEnergy, *api.MockChargeRater) {
		t.Helper()
		var err error
		serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
		require.NoError(t, err)
		db, err := session.NewStore("foo", serverdb.Instance)
		require.NoError(t, err)

		ctrl := gomock.NewController(t)
		mm := api.NewMockMeter(ctrl)
		me := api.NewMockMeterEnergy(ctrl)
		rater := api.NewMockChargeRater(ctrl)

		type EnergyDecorator struct {
			api.Meter
			api.MeterEnergy
		}

		cm := &EnergyDecorator{
			Meter:       mm,
			MeterEnergy: me,
		}

		lp := &Loadpoint{
			log:         util.NewLogger("foo"),
			clock:       clock.NewMock(),
			db:          db,
			chargeRater: rater,
			chargeMeter: cm,
		}
		return lp, me, rater
	}

	t.Run("corrects session when ChargedEnergy increased", func(t *testing.T) {
		lp, me, rater := setup(t)

		me.EXPECT().TotalEnergy().Return(9157.3, nil)
		lp.createSession()
		lp.session.Created = lp.clock.Now()

		lp.energyMetrics.Update(15.3)
		me.EXPECT().TotalEnergy().Return(9164.0, nil)
		lp.stopSession()
		require.Equal(t, 15.3, lp.session.ChargedEnergy)

		rater.EXPECT().ChargedEnergy().Return(16.2, nil)
		me.EXPECT().TotalEnergy().Return(9173.5, nil)
		lp.finalizeSessionEnergy()

		assert.Equal(t, 16.2, lp.session.ChargedEnergy)
		assert.Equal(t, 9173.5, *lp.session.MeterStop)
		assert.Equal(t, 9157.3, *lp.session.MeterStart)
	})

	t.Run("no-op when ChargedEnergy unchanged", func(t *testing.T) {
		lp, me, rater := setup(t)

		me.EXPECT().TotalEnergy().Return(9154.4, nil)
		lp.createSession()
		lp.session.Created = lp.clock.Now()

		lp.energyMetrics.Update(15.3)
		me.EXPECT().TotalEnergy().Return(9164.0, nil)
		lp.stopSession()
		require.Equal(t, 15.3, lp.session.ChargedEnergy)

		rater.EXPECT().ChargedEnergy().Return(15.3, nil)
		lp.finalizeSessionEnergy()

		assert.Equal(t, 15.3, lp.session.ChargedEnergy)
		assert.Equal(t, 9164.0, *lp.session.MeterStop)
	})

	t.Run("no-op when session nil or uncreated", func(t *testing.T) {
		lp, _, _ := setup(t)

		lp.session = nil
		assert.NotPanics(t, func() { lp.finalizeSessionEnergy() })

		lp.session = &session.Session{}
		assert.NotPanics(t, func() { lp.finalizeSessionEnergy() })
	})
}

func TestSplitSession(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}

	cm := &EnergyDecorator{Meter: mm, MeterEnergy: me}

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock,
		db:          db,
		chargeMeter: cm,
		status:      api.StatusC,
	}

	// the charger reports exactly what evcc already counted, so the
	// finalizeSessionEnergy call inside splitSession is a no-op
	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().DoAndReturn(func() (float64, error) {
		return lp.energyMetrics.TotalWh() / 1e3, nil
	}).AnyTimes()
	lp.chargeRater = rater

	// first session starts at meter 10.0 kWh
	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	// charge 5 kWh
	clock.Add(time.Hour)
	lp.energyMetrics.Update(5.0)

	// split at meter 15.0 - stopSession and createSession read the meter once each
	me.EXPECT().TotalEnergy().Return(15.0, nil).Times(2)
	lp.splitSession(nil, false)

	require.NotNil(t, lp.session)
	assert.False(t, lp.session.Created.IsZero(), "new session must be marked started while charging")
	assert.Zero(t, lp.getChargedEnergy(), "energy counter must restart at zero")

	// charge another 3 kWh on the new session
	clock.Add(time.Hour)
	lp.energyMetrics.Update(3.0)

	me.EXPECT().TotalEnergy().Return(18.0, nil)
	lp.stopSession()

	s, err := db.Sessions()
	require.NoError(t, err)
	require.Len(t, s, 2)

	assert.Equal(t, 5.0, s[0].ChargedEnergy)
	assert.Equal(t, 10.0, *s[0].MeterStart)
	assert.Equal(t, 15.0, *s[0].MeterStop)

	assert.Equal(t, 3.0, s[1].ChargedEnergy)
	assert.Equal(t, 15.0, *s[1].MeterStart)
	assert.Equal(t, 18.0, *s[1].MeterStop)
}

func TestSplitSessionChargeDuration(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}

	cm := &EnergyDecorator{Meter: mm, MeterEnergy: me}

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock,
		db:          db,
		chargeMeter: cm,
		status:      api.StatusC,
	}

	// the charger reports exactly what evcc already counted, so the
	// finalizeSessionEnergy calls inside splitSession and
	// evVehicleDisconnectHandler are no-ops
	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().DoAndReturn(func() (float64, error) {
		return lp.energyMetrics.TotalWh() / 1e3, nil
	}).AnyTimes()
	lp.chargeRater = rater

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	// charger-provided timer counts from plug-in and cannot be reset
	lp.chargeDuration = 2 * time.Hour
	lp.energyMetrics.Update(5.0)

	me.EXPECT().TotalEnergy().Return(15.0, nil).Times(2)
	lp.splitSession(nil, false)

	assert.Equal(t, 2*time.Hour, lp.chargeDurationOffset)

	// charger timer keeps running: one more hour after the split
	lp.chargeDuration = 3 * time.Hour
	lp.energyMetrics.Update(3.0)

	me.EXPECT().TotalEnergy().Return(18.0, nil)
	lp.stopSession()

	require.NotNil(t, lp.session.ChargeDuration)
	assert.Equal(t, time.Hour, *lp.session.ChargeDuration, "second leg must only count time since the split")

	// disconnect clears the offset
	lp.evVehicleDisconnectHandler()
	assert.Zero(t, lp.chargeDurationOffset, "disconnect must clear a stale split offset")
}

// TestSplitSessionResettableChargeTimer covers the branch of splitSession where
// lp.chargeTimer does implement wrapper.ChargeResetter (e.g. wrapper.ChargeTimer,
// used for chargers without their own api.ChargeTimer): the timer itself gets
// reset and no offset is needed. TestSplitSessionChargeDuration only exercises
// the opposite branch (lp.chargeTimer is nil, so not a ChargeResetter).
func TestSplitSessionResettableChargeTimer(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	clock := clock.NewMock()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}

	cm := &EnergyDecorator{Meter: mm, MeterEnergy: me}

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock,
		db:          db,
		chargeMeter: cm,
		chargeTimer: wrapper.NewChargeTimer(),
		status:      api.StatusC,
	}

	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().DoAndReturn(func() (float64, error) {
		return lp.energyMetrics.TotalWh() / 1e3, nil
	}).AnyTimes()
	lp.chargeRater = rater

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	// simulate a stale offset from an earlier split to prove the ResetCharge
	// branch actually runs, rather than just reading back a zero default
	lp.chargeDurationOffset = 99 * time.Hour

	me.EXPECT().TotalEnergy().Return(15.0, nil).Times(2)
	lp.splitSession(nil, false)

	assert.Zero(t, lp.chargeDurationOffset, "resettable charge timer must not need a duration offset")
}

// splitTestMeter returns a charge meter decorated with MeterEnergy.
func splitTestMeter(mm api.Meter, me api.MeterEnergy) api.Meter {
	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}
	return &EnergyDecorator{Meter: mm, MeterEnergy: me}
}

// TestSplitSessionDetachVehicle covers the guest-vehicle path: the split must
// leave the new session without a vehicle. createSession copies the currently
// active vehicle's title, and setActiveVehicle(nil) does not clear it again, so
// the split has to guarantee this itself.
func TestSplitSessionDetachVehicle(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	v := api.NewMockVehicle(ctrl)
	expectVehiclePublish(v)

	lp := NewLoadpoint(util.NewLogger("foo"), settings.NewDatabaseSettingsAdapter("foo"))
	lp.clock = clock.NewMock()
	lp.db = db
	lp.chargeMeter = splitTestMeter(mm, me)
	lp.status = api.StatusC

	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().DoAndReturn(func() (float64, error) {
		return lp.energyMetrics.TotalWh() / 1e3, nil
	}).AnyTimes()
	lp.chargeRater = rater

	uiChan, pushChan, lpChan := createChannels(t)
	attachChannels(lp, uiChan, pushChan, lpChan)

	lp.setActiveVehicle(v)

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))
	require.Equal(t, "target", lp.session.Vehicle)

	lp.energyMetrics.Update(5.0)

	me.EXPECT().TotalEnergy().Return(15.0, nil).AnyTimes()
	lp.splitSession(nil, true)

	assert.Nil(t, lp.GetVehicle(), "vehicle must be detached from the loadpoint")
	require.NotNil(t, lp.session)
	assert.Empty(t, lp.session.Vehicle, "guest session must not inherit the previous vehicle")

	s, err := db.Sessions()
	require.NoError(t, err)
	require.Len(t, s, 2)
	assert.Equal(t, "target", s[0].Vehicle, "finished leg keeps its vehicle")
	assert.Empty(t, s[1].Vehicle, "new leg must be unassigned")
}

// TestSplitSessionKeepsVehicle guards the two unchanged meanings of the
// endpoint: split without a vehicle keeps the current one.
func TestSplitSessionKeepsVehicle(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	v := api.NewMockVehicle(ctrl)
	expectVehiclePublish(v)

	lp := NewLoadpoint(util.NewLogger("foo"), settings.NewDatabaseSettingsAdapter("foo"))
	lp.clock = clock.NewMock()
	lp.db = db
	lp.chargeMeter = splitTestMeter(mm, me)
	lp.status = api.StatusC

	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().DoAndReturn(func() (float64, error) {
		return lp.energyMetrics.TotalWh() / 1e3, nil
	}).AnyTimes()
	lp.chargeRater = rater

	uiChan, pushChan, lpChan := createChannels(t)
	attachChannels(lp, uiChan, pushChan, lpChan)

	lp.setActiveVehicle(v)

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	lp.energyMetrics.Update(5.0)

	me.EXPECT().TotalEnergy().Return(15.0, nil).AnyTimes()
	lp.splitSession(nil, false)

	assert.Equal(t, v, lp.GetVehicle(), "vehicle must be kept")
	require.NotNil(t, lp.session)
	assert.Equal(t, "target", lp.session.Vehicle, "new leg keeps the vehicle")
}

// TestSplitSessionQueuesEverySplit covers that a second split request is not
// silently dropped. lp.addTask deduplicates by reflect.Value.Pointer(), which
// for closures is the code pointer of the enclosing function literal and hence
// identical for every split - regardless of the vehicle bound to it.
func TestSplitSessionQueuesEverySplit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	v1 := api.NewMockVehicle(ctrl)
	v2 := api.NewMockVehicle(ctrl)

	lp := &Loadpoint{
		log:   util.NewLogger("foo"),
		clock: clock.NewMock(),
		tasks: util.NewQueue[Task](),
	}

	lp.SplitSession(v1, false)
	lp.SplitSession(v2, false)

	assert.Equal(t, 2, lp.tasks.Size(), "second split must not be deduplicated away")
}

// TestStopSessionChargeDurationOffset covers charger-provided charge timers
// that jump backwards between split and stop (wallbox reboot, modbus
// reconnect). The stale offset must not be mirrored into a plausible looking
// but wrong duration.
func TestStopSessionChargeDurationOffset(t *testing.T) {
	tc := []struct {
		name           string
		duration       time.Duration
		offset         time.Duration
		expectDuration time.Duration
	}{
		{"no offset", 3 * time.Hour, 0, 3 * time.Hour},
		{"regular offset", 3 * time.Hour, 2 * time.Hour, time.Hour},
		{"counter reset below offset", 10 * time.Minute, 2 * time.Hour, 10 * time.Minute},
		{"counter equals offset", 2 * time.Hour, 2 * time.Hour, 0},
		{"negative counter", -5 * time.Minute, 0, 0},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
			require.NoError(t, err)

			db, err := session.NewStore("foo", serverdb.Instance)
			require.NoError(t, err)

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mm := api.NewMockMeter(ctrl)
			me := api.NewMockMeterEnergy(ctrl)
			me.EXPECT().TotalEnergy().Return(10.0, nil).AnyTimes()

			lp := &Loadpoint{
				log:         util.NewLogger("foo"),
				clock:       clock.NewMock(),
				db:          db,
				chargeMeter: splitTestMeter(mm, me),
				status:      api.StatusC,
			}

			lp.createSession()
			lp.updateSession(sessionStart(lp))

			lp.chargeDuration = tc.duration
			lp.chargeDurationOffset = tc.offset
			lp.stopSession()

			require.NotNil(t, lp.session.ChargeDuration)
			assert.Equal(t, tc.expectDuration, *lp.session.ChargeDuration)
		})
	}
}

// TestSplitSessionWarnsOnNonResettableRater covers the silent-miscount case:
// if lp.chargeRater is not a wrapper.ChargeResetter, ResetCharge is a no-op and
// the finished leg's energy is counted again in the new one. That must be
// loudly visible in the log.
func TestSplitSessionWarnsOnNonResettableRater(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)
	me.EXPECT().TotalEnergy().Return(10.0, nil).AnyTimes()

	log := util.NewLogger("splitwarn")
	var buf bytes.Buffer
	log.WARN.SetOutput(&buf)

	lp := &Loadpoint{
		log:         log,
		clock:       clock.NewMock(),
		db:          db,
		chargeMeter: splitTestMeter(mm, me),
		status:      api.StatusC,
	}

	// api.MockChargeRater does not implement wrapper.ChargeResetter
	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().Return(0.0, nil).AnyTimes()
	lp.chargeRater = rater

	lp.createSession()
	lp.updateSession(sessionStart(lp))
	lp.splitSession(nil, false)

	assert.Contains(t, buf.String(), "cannot be reset", "non-resettable charge rater must be warned about")

	// a resettable rater must not warn
	buf.Reset()
	lp.chargeRater = wrapper.NewChargeRater(log, mm)
	lp.splitSession(nil, false)

	assert.Empty(t, buf.String(), "resettable charge rater must not warn")
}

// TestSplitSessionFinalizesEnergy covers the energy window between the last
// publishChargeProgress and the split: tasks run before publishChargeProgress,
// so lp.energyMetrics lags one cycle behind while the charge meter is read
// fresh. Without re-reading the charger's own energy, that window belongs to
// neither leg.
func TestSplitSessionFinalizesEnergy(t *testing.T) {
	var err error
	serverdb.Instance, err = serverdb.New("sqlite", ":memory:")
	require.NoError(t, err)

	db, err := session.NewStore("foo", serverdb.Instance)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mm := api.NewMockMeter(ctrl)
	me := api.NewMockMeterEnergy(ctrl)

	lp := &Loadpoint{
		log:         util.NewLogger("foo"),
		clock:       clock.NewMock(),
		db:          db,
		chargeMeter: splitTestMeter(mm, me),
		status:      api.StatusC,
	}

	// the charger has already delivered 5.5kWh while lp.energyMetrics still
	// holds the previous cycle's 5.0kWh
	rater := api.NewMockChargeRater(ctrl)
	rater.EXPECT().ChargedEnergy().Return(5.5, nil).AnyTimes()
	lp.chargeRater = rater

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	lp.energyMetrics.Update(5.0)

	me.EXPECT().TotalEnergy().Return(15.5, nil).AnyTimes()
	lp.splitSession(nil, false)

	s, err := db.Sessions()
	require.NoError(t, err)
	require.NotEmpty(t, s)
	assert.Equal(t, 5.5, s[0].ChargedEnergy, "finished leg must include the energy since the last publish")
	assert.Zero(t, lp.getChargedEnergy(), "new leg must start at zero")
}
