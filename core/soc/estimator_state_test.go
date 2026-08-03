package soc

import (
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// newTestEstimator builds an estimator for an 8.5 kWh vehicle, which yields a
// 10 kWh virtual capacity and 100 Wh per soc step at the default 85% efficiency.
func newTestEstimator(t *testing.T) *Estimator {
	t.Helper()

	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(8.5).AnyTimes()

	return NewEstimator(util.NewLogger("test"), api.NewMockCharger(ctrl), vehicle)
}

func TestStateReportsInternals(t *testing.T) {
	ce := newTestEstimator(t)

	soc := 20.0
	ce.Soc(&soc, 0)   // rebase branch, anchors prevSoc at 20
	ce.Soc(&soc, 500) // estimating branch, 500 Wh = 5 percentage points

	st := ce.State()
	assert.Equal(t, 25.0, st.VehicleSoc)
	assert.Equal(t, 20.0, st.FetchedSoc)
	assert.Equal(t, 20.0, st.PrevSoc)
	assert.Equal(t, 0.0, st.PrevChargedEnergy)
	assert.Equal(t, 100.0, st.EnergyPerSocStep)
	assert.Equal(t, 10000.0, st.VirtualCapacity)
	assert.False(t, st.Learned, "gradient never updated")
}

func TestSetSocSurvivesFurtherPolls(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0) // anchor at 15

	require.NoError(t, ce.SetSoc(20))
	assert.Equal(t, 20.0, ce.State().VehicleSoc)

	// source keeps reporting the stale 15 while another 300 Wh flow in
	assert.Equal(t, 20.0, ce.Soc(&source, 0), "override must hold across a poll")
	assert.InDelta(t, 23.0, ce.Soc(&source, 300), 0.001, "charged energy adds on top")
}

func TestSetSocLeavesAnchorFieldsAlone(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0)
	before := ce.State()

	require.NoError(t, ce.SetSoc(20))
	after := ce.State()

	assert.Equal(t, before.PrevSoc, after.PrevSoc, "prevSoc must not move, or the next poll rebases")
	assert.Equal(t, before.InitialSoc, after.InitialSoc)
	assert.Equal(t, before.InitialEnergy, after.InitialEnergy)
	assert.Equal(t, before.EnergyPerSocStep, after.EnergyPerSocStep)
}

func TestSetSocExpiresOnNewSourceValue(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0)
	require.NoError(t, ce.SetSoc(20))

	// vehicle comes back online and reports something genuinely new
	fresh := 42.0
	assert.Equal(t, 42.0, ce.Soc(&fresh, 0), "override must not survive a changed source value")
	assert.Equal(t, 42.0, ce.Soc(&fresh, 0))
}

func TestSetSocRejectsOutOfRange(t *testing.T) {
	ce := newTestEstimator(t)
	source := 15.0
	ce.Soc(&source, 0)

	assert.Error(t, ce.SetSoc(-1))
	assert.Error(t, ce.SetSoc(101))
}

func TestShiftEnergy(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0)

	require.NoError(t, ce.ShiftEnergy(500)) // 500 Wh at 100 Wh/% = 5 points
	assert.Equal(t, 20.0, ce.State().VehicleSoc)
	assert.Equal(t, 20.0, ce.Soc(&source, 0))
}
