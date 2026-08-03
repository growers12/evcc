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

func splitTestMeter(mm api.Meter, me api.MeterEnergy) api.Meter {
	type EnergyDecorator struct {
		api.Meter
		api.MeterEnergy
	}
	return &EnergyDecorator{Meter: mm, MeterEnergy: me}
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
