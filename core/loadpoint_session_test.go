package core

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/session"
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

	// first session starts at meter 10.0 kWh
	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	// charge 5 kWh
	clock.Add(time.Hour)
	lp.energyMetrics.Update(5.0)

	// split at meter 15.0 - stopSession and createSession read the meter once each
	me.EXPECT().TotalEnergy().Return(15.0, nil).Times(2)
	lp.splitSession(nil)

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

	me.EXPECT().TotalEnergy().Return(10.0, nil)
	lp.createSession()
	lp.updateSession(sessionStart(lp))

	// charger-provided timer counts from plug-in and cannot be reset
	lp.chargeDuration = 2 * time.Hour
	lp.energyMetrics.Update(5.0)

	me.EXPECT().TotalEnergy().Return(15.0, nil).Times(2)
	lp.splitSession(nil)

	assert.Equal(t, 2*time.Hour, lp.chargeDurationOffset)

	// charger timer keeps running: one more hour after the split
	lp.chargeDuration = 3 * time.Hour
	lp.energyMetrics.Update(3.0)

	me.EXPECT().TotalEnergy().Return(18.0, nil)
	lp.stopSession()

	require.NotNil(t, lp.session.ChargeDuration)
	assert.Equal(t, time.Hour, *lp.session.ChargeDuration, "second leg must only count time since the split")

	// disconnect clears the offset
	lp.chargeDurationOffset = 0
	assert.Zero(t, lp.chargeDurationOffset)
}
