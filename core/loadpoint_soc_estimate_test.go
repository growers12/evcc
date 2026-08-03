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
