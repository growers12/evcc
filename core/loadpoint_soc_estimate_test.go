package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	coresettings "github.com/evcc-io/evcc/core/settings"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestSocEstimateSoc(t *testing.T) {
	se := SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4823.5, EnergyPerSocStep: 964.7}
	assert.InDelta(t, 20.0, se.soc(), 0.01)
}

func TestSocEstimatePlausible(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	tc := []struct {
		name      string
		se        SocEstimate
		plausible bool
	}{
		{
			"fresh record",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4823, EnergyPerSocStep: 964.7, Updated: now.Add(-time.Hour)},
			true,
		},
		{
			"older than 24h",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4823, EnergyPerSocStep: 964.7, Updated: now.Add(-25 * time.Hour)},
			false,
		},
		{
			"offset beyond 50 points",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 60000, EnergyPerSocStep: 964.7, Updated: now.Add(-time.Hour)},
			false,
		},
		{
			"no gradient",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4823, EnergyPerSocStep: 0, Updated: now.Add(-time.Hour)},
			false,
		},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.plausible, tc.se.plausible(now))
		})
	}
}

func TestSocEstimateRoundtrip(t *testing.T) {
	se := SocEstimate{
		AnchorSoc:         15,
		EnergySinceAnchor: 4823.5,
		EnergyPerSocStep:  964.7,
		Samples:           3,
		OdometerAtAnchor:  28011,
		Updated:           time.Date(2026, 8, 3, 6, 55, 0, 0, time.UTC),
	}

	require.NoError(t, saveSocEstimate("test:1", se))

	got, ok := loadSocEstimate("test:1")
	assert.True(t, ok)
	assert.Equal(t, se.AnchorSoc, got.AnchorSoc)
	assert.Equal(t, se.EnergySinceAnchor, got.EnergySinceAnchor)
	assert.Equal(t, se.Samples, got.Samples)

	_, ok = loadSocEstimate("test:does-not-exist")
	assert.False(t, ok)
}

func TestUpdateSocEstimateTracksAnchor(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.socEstimateVehicle = "test:2"

	source := 15.0
	lp.socEstimator.Soc(&source, 0)
	lp.updateSocEstimate(lp.socEstimator)

	se, ok := loadSocEstimate("test:2")
	require.True(t, ok)
	assert.Equal(t, 15.0, se.AnchorSoc)
	assert.Equal(t, 0.0, se.EnergySinceAnchor)

	// 300 Wh into the session, source still frozen
	lp.socEstimator.Soc(&source, 300)
	lp.updateSocEstimate(lp.socEstimator)

	se, _ = loadSocEstimate("test:2")
	assert.Equal(t, 15.0, se.AnchorSoc, "anchor stays put while the source is frozen")
	assert.Equal(t, 300.0, se.EnergySinceAnchor)
}

func TestUpdateSocEstimateAccumulatesAcrossSessions(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimateVehicle = "test:3"

	// first session delivers 300 Wh
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	source := 15.0
	lp.socEstimator.Soc(&source, 0)
	lp.socEstimator.Soc(&source, 300)
	lp.updateSocEstimate(lp.socEstimator)

	// unplug and replug: fresh estimator, session counter back to zero
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.restoreSocEstimate()
	lp.socEstimator.Soc(&source, 0)
	lp.socEstimator.Soc(&source, 200)
	lp.updateSocEstimate(lp.socEstimator)

	se, _ := loadSocEstimate("test:3")
	assert.Equal(t, 500.0, se.EnergySinceAnchor, "energy accumulates across the session boundary")
}

func TestRestoreDropsOffsetWhenSourceMovedOn(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimateVehicle = "test:4"

	require.NoError(t, saveSocEstimate("test:4", SocEstimate{
		AnchorSoc: 15, EnergySinceAnchor: 500, EnergyPerSocStep: 100, Updated: time.Now(),
	}))

	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.restoreSocEstimate()
	assert.Equal(t, 20.0, lp.socEstimator.State().VehicleSoc)

	// the car was driven while evcc was down and now reports a real value.
	// restoreSocEstimate does not check this — the estimator's rebase branch
	// does, because Restore anchored prevSoc at 15.
	fresh := 42.0
	assert.Equal(t, 42.0, lp.socEstimator.Soc(&fresh, 0), "stale offset must not survive a moved source")
}

func TestLearnGradient(t *testing.T) {
	// 8.5 kWh nominal => 100 Wh/% initial, 10000 Wh virtual capacity
	const capacity = 8.5

	tc := []struct {
		name     string
		se       SocEstimate
		newSoc   float64
		odometer float64
		learned  bool
		expected float64
	}{
		{
			"clean 40 point rise",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4800, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			55, 28013, true,
			// measured 4800/40 = 120, EMA 0.7*100 + 0.3*120 = 106
			106,
		},
		{
			"rise too small",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 800, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			23, 28013, false, 100,
		},
		{
			"no energy since anchor",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 0, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			55, 28013, false, 100,
		},
		{
			"drove too far before reporting",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4800, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			55, 28040, false, 100,
		},
		{
			"result beyond twice nominal",
			SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 40000, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			55, 28013, false, 100,
		},
		{
			"soc dropped",
			SocEstimate{AnchorSoc: 55, EnergySinceAnchor: 4800, EnergyPerSocStep: 100, OdometerAtAnchor: 28011},
			15, 28013, false, 100,
		},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			got, learned := tc.se.learn(tc.newSoc, tc.odometer, capacity)

			assert.Equal(t, tc.learned, learned)
			assert.InDelta(t, tc.expected, got.EnergyPerSocStep, 0.01)

			// the anchor is re-set either way, or a missed learning moment
			// would block the record forever
			assert.Equal(t, tc.newSoc, got.AnchorSoc)
			assert.Equal(t, 0.0, got.EnergySinceAnchor)
			assert.Equal(t, tc.odometer, got.OdometerAtAnchor)
		})
	}
}

func TestLearnCountsSamples(t *testing.T) {
	se := SocEstimate{AnchorSoc: 15, EnergySinceAnchor: 4800, EnergyPerSocStep: 100, OdometerAtAnchor: 28011, Samples: 2}

	got, learned := se.learn(55, 28013, 8.5)
	assert.True(t, learned)
	assert.Equal(t, 3, got.Samples)
}

// TestUpdateSocEstimateLearnsAcrossSessionBoundary exercises learn() through
// updateSocEstimate end to end, guarding against the bookkeeping block below
// the rebase silently overwriting a gradient that was just learned, and that
// it keeps holding on later polls rather than being clobbered one cycle
// later by the estimator's own (still-stale) gradient — see production's
// call pattern in publishSocAndRange: socEstimator.Soc(socR,
// lp.GetChargedEnergy()) followed by lp.updateSocEstimate(socEstimator),
// both reading the same lp.GetChargedEnergy(). The test drives
// lp.energyMetrics directly (rather than passing ad-hoc numbers to Soc)
// so that coupling is real, not assumed.
func TestUpdateSocEstimateLearnsAcrossSessionBoundary(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.vehicle = vehicle // vehicleCapacity() reads the active vehicle via GetVehicle()
	lp.socEstimateVehicle = "test:6"
	lp.socEstimateOdometer = 28011

	// session 1: anchor at 15%, source frozen, 4800 Wh delivered
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	source := 15.0
	lp.socEstimator.Soc(&source, lp.GetChargedEnergy())
	lp.updateSocEstimate(lp.socEstimator)
	lp.energyMetrics.Update(4.8) // 4800 Wh
	lp.socEstimator.Soc(&source, lp.GetChargedEnergy())
	lp.updateSocEstimate(lp.socEstimator)

	// unplug, drive 2km (within the guard), replug: fresh estimator and fresh
	// session energy counter, restore folds the persisted anchor back in
	lp.socEstimateOdometer = 28013
	lp.energyMetrics.Reset()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.restoreSocEstimate()

	// the car finally reports the real value: a clean 40 point rise
	source = 55.0
	lp.socEstimator.Soc(&source, lp.GetChargedEnergy())
	lp.updateSocEstimate(lp.socEstimator)

	se, ok := loadSocEstimate("test:6")
	require.True(t, ok)
	// measured 4800/40 = 120, EMA 0.7*100 + 0.3*120 = 106
	assert.InDelta(t, 106.0, se.EnergyPerSocStep, 0.01, "learned gradient must survive the persisted record")
	assert.Equal(t, 1, se.Samples)

	// two more polls after the learning call: the source stays caught up at
	// 55% while more energy is delivered. Both the persisted record and the
	// live estimator must keep reading the learned gradient, not the stale
	// pre-learn value.
	lp.energyMetrics.Update(0.05) // +50 Wh
	lp.socEstimator.Soc(&source, lp.GetChargedEnergy())
	lp.updateSocEstimate(lp.socEstimator)

	lp.energyMetrics.Update(0.10) // +50 Wh more
	lp.socEstimator.Soc(&source, lp.GetChargedEnergy())
	lp.updateSocEstimate(lp.socEstimator)

	se, ok = loadSocEstimate("test:6")
	require.True(t, ok)
	assert.InDelta(t, 106.0, se.EnergyPerSocStep, 0.01, "gradient must survive polls after the learning call")
	assert.Equal(t, 1, se.Samples, "no further learning happens while the source stays caught up")
	assert.InDelta(t, 106.0, lp.socEstimator.State().EnergyPerSocStep, 0.01, "the live estimator must also carry the learned gradient")
}

func TestSetSocEstimateWithoutEstimator(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	assert.Error(t, lp.SetSocEstimate(20))
	assert.Error(t, lp.ShiftSocEstimate(1.5))

	_, _, ok := lp.GetSocEstimate()
	assert.False(t, ok)
}

// TestSetSocEstimateOutOfRange covers the synchronous validation in
// SetSocEstimate, which must reject before ever touching the task queue -
// an out-of-range target must not reach the estimator at all.
func TestSetSocEstimateOutOfRange(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)

	assert.Error(t, lp.SetSocEstimate(-1))
	assert.Error(t, lp.SetSocEstimate(101))
	assert.Equal(t, 0, lp.tasks.Size(), "an out-of-range target must never reach the task queue")
}

// TestSetSocEstimateQueuesTask and TestShiftSocEstimateQueuesTask mirror
// TestSplitSessionQueuesEverySplit: they check that both methods use
// enqueueTask rather than addTask, i.e. that a second call is not silently
// dropped because it shares the same closure's code pointer. The tasks are
// deliberately left unrun - running them would exercise updateSocEstimate,
// which persists via saveSocEstimate/settings.SetJson and only touches the
// in-memory settings slice, so that part would be safe; but there is no
// reason to actually execute the closures here, only to prove they were
// enqueued.
func TestSetSocEstimateQueuesTask(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)

	require.NoError(t, lp.SetSocEstimate(20))
	require.NoError(t, lp.SetSocEstimate(40))

	assert.Equal(t, 2, lp.tasks.Size(), "second call must not be deduplicated away")
}

func TestShiftSocEstimateQueuesTask(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)

	require.NoError(t, lp.ShiftSocEstimate(1.5))
	require.NoError(t, lp.ShiftSocEstimate(-2.0))

	assert.Equal(t, 2, lp.tasks.Size(), "second call must not be deduplicated away")
}

// TestClearSocEstimateWithoutVehicle covers the synchronous guard in
// ClearSocEstimate. This deliberately does not exercise the queued
// deleteSocEstimate call: settings.Delete reaches the package-level
// db.Instance without a nil check as soon as the key already exists in the
// in-memory settings slice, and a bare `go test ./core/` has no database.
// See the ClearSocEstimate doc comment / task report for the full reasoning.
func TestClearSocEstimateWithoutVehicle(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	assert.ErrorIs(t, lp.ClearSocEstimate(), ErrNoSocEstimator)
	assert.Equal(t, 0, lp.tasks.Size(), "without a vehicle name there is nothing to enqueue")
}

// TestClearSocEstimateQueuesTask proves ClearSocEstimate enqueues rather than
// dedicates via addTask, without running the closure (see the comment on
// TestClearSocEstimateWithoutVehicle for why the delete itself stays
// untested at this level).
func TestClearSocEstimateQueuesTask(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))
	lp.socEstimateVehicle = "test:clear"

	require.NoError(t, lp.ClearSocEstimate())
	require.NoError(t, lp.ClearSocEstimate())

	assert.Equal(t, 2, lp.tasks.Size(), "second call must not be deduplicated away")
}

// TestGetSocEstimateReturnsState covers the ok=true path: a live estimator
// plus a persisted record. loadSocEstimate only reads the in-memory settings
// slice (settings.String), so this does not touch db.Instance.
func TestGetSocEstimateReturnsState(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.socEstimateVehicle = "test:get"

	require.NoError(t, saveSocEstimate("test:get", SocEstimate{AnchorSoc: 33, EnergyPerSocStep: 100}))

	st, se, ok := lp.GetSocEstimate()
	assert.True(t, ok)
	assert.Equal(t, 33.0, se.AnchorSoc, "the persisted record must be returned alongside the live state")
	// a fresh estimator with no polls yet reports vehicleSoc 0, regardless of
	// the persisted record above - GetSocEstimate does not merge the two
	assert.Equal(t, 0.0, st.VehicleSoc)
}

func TestRestoreSocEstimateDropsExpiredOffset(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("test"), coresettings.NewDatabaseSettingsAdapter("test."))

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()
	lp.socEstimateVehicle = "test:5"

	require.NoError(t, saveSocEstimate("test:5", SocEstimate{
		AnchorSoc: 15, EnergySinceAnchor: 500, EnergyPerSocStep: 100, Updated: time.Now().Add(-25 * time.Hour),
	}))

	lp.socEstimator = soc.NewEstimator(lp.log, api.NewMockCharger(ctrl), vehicle)
	lp.restoreSocEstimate()

	st := lp.socEstimator.State()
	assert.Equal(t, 15.0, st.VehicleSoc, "offset discarded: vehicle soc falls back to the anchor")
	assert.Equal(t, 100.0, st.EnergyPerSocStep, "gradient survives even when the offset is discarded")
}
