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
	assert.Equal(t, 500.0, st.ChargedEnergy, "the counter the estimate was computed against")
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

// TestSetSocRejectsBelowSource covers the silent-revert case: an anchor below
// the source value pushes prevChargedEnergy past the counter, so the next poll
// sees energyDelta < 0, rebases and drops the override. Answering success and
// reverting one poll later is worse than refusing, so SetSoc refuses.
func TestSetSocRejectsBelowSource(t *testing.T) {
	ce := newTestEstimator(t)

	source := 40.0
	ce.Soc(&source, 0)

	require.Error(t, ce.SetSoc(30), "below the vehicle's own value")
	assert.Equal(t, 40.0, ce.State().VehicleSoc, "the rejected target must not be applied")
	assert.Equal(t, 40.0, ce.Soc(&source, 0))

	require.NoError(t, ce.SetSoc(40), "exactly the source value is allowed")
}

// TestShiftEnergyRejectsBelowSource is the operator's obvious move after
// over-booking energy - it must fail loudly rather than revert on the next poll
func TestShiftEnergyRejectsBelowSource(t *testing.T) {
	ce := newTestEstimator(t)

	source := 40.0
	ce.Soc(&source, 0)
	require.NoError(t, ce.ShiftEnergy(500)) // 45%

	require.NoError(t, ce.ShiftEnergy(-300), "back down to 42%, still above the source")
	assert.InDelta(t, 42.0, ce.State().VehicleSoc, 0.001)

	require.Error(t, ce.ShiftEnergy(-500), "would land below the source value")
	assert.InDelta(t, 42.0, ce.State().VehicleSoc, 0.001)
}

func TestShiftEnergy(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0)

	require.NoError(t, ce.ShiftEnergy(500)) // 500 Wh at 100 Wh/% = 5 points
	assert.Equal(t, 20.0, ce.State().VehicleSoc)
	assert.Equal(t, 20.0, ce.Soc(&source, 0))
}

// TestResetOverrideFollowsSourceAgain covers what DELETE /soc has to do to the
// running estimator. Dropping the persisted record alone leaves the offset in
// prevChargedEnergy, and the next poll writes it straight back.
func TestResetOverrideFollowsSourceAgain(t *testing.T) {
	ce := newTestEstimator(t)

	source := 15.0
	ce.Soc(&source, 0)   // rebase, anchors at 15
	ce.Soc(&source, 500) // 20% - 15 from the source, 5 from 500 Wh
	require.Equal(t, 20.0, ce.State().VehicleSoc)

	require.NoError(t, ce.SetSoc(30))
	assert.Equal(t, 30.0, ce.State().VehicleSoc)

	require.NoError(t, ce.ResetOverride())
	assert.Equal(t, 15.0, ce.State().VehicleSoc, "the estimate follows the source again")
	assert.Equal(t, 15.0, ce.Soc(&source, 500), "and keeps doing so on the next poll")

	// energy delivered after the reset still counts, the estimator is not off
	assert.InDelta(t, 17.0, ce.Soc(&source, 700), 0.001)
}

// TestResetOverrideOnRestoredEstimator guards the case where DELETE arrives
// before the first poll: fetchedSoc is then the restored anchor rather than
// zero, so the reset must not drag the estimate down to 0%.
func TestResetOverrideOnRestoredEstimator(t *testing.T) {
	ce := newTestEstimator(t)
	ce.Restore(15, 500, 100, 0, true)

	require.NoError(t, ce.ResetOverride())
	assert.Equal(t, 15.0, ce.State().VehicleSoc)
}

func TestRestoreSeedsEstimate(t *testing.T) {
	ce := newTestEstimator(t)

	// 500 Wh since the anchor at 15% => 20% at 100 Wh per point
	ce.Restore(15, 500, 100, 0, true)

	st := ce.State()
	assert.Equal(t, 20.0, st.VehicleSoc)
	assert.Equal(t, 15.0, st.PrevSoc, "prevSoc must equal the anchor")
	assert.True(t, st.Learned)
}

func TestRestoreFirstPollDoesNotRebase(t *testing.T) {
	ce := newTestEstimator(t)
	ce.Restore(15, 500, 100, 0, true)

	// a fresh estimator has prevSoc 0; without Restore setting it to the anchor
	// this first poll would take the rebase branch and wipe the restored offset
	source := 15.0
	assert.Equal(t, 20.0, ce.Soc(&source, 0), "restored estimate must survive the first poll")
	assert.InDelta(t, 22.0, ce.Soc(&source, 200), 0.001)
}

func TestRestoreKeepsGradientWithoutOffset(t *testing.T) {
	ce := newTestEstimator(t)

	ce.Restore(15, 0, 250, 0, true)

	st := ce.State()
	assert.Equal(t, 250.0, st.EnergyPerSocStep, "gradient is restored unconditionally")
	assert.Equal(t, 15.0, st.VehicleSoc, "no energy since anchor means no offset")
}

func TestRestoreWithRunningSessionEnergy(t *testing.T) {
	ce := newTestEstimator(t)

	// evcc restarted mid-session: the counter already stands at 800 Wh
	ce.Restore(15, 500, 100, 800, true)

	source := 15.0
	assert.Equal(t, 20.0, ce.Soc(&source, 800), "anchor is relative to the current counter")
}

func TestRestoreWithoutUsableGradientKeepsAnchor(t *testing.T) {
	// a vehicle with 0 configured capacity makes the estimator's own
	// gradient 0 too, so newTestEstimator (8.5 kWh) can't reproduce this case
	ctrl := gomock.NewController(t)
	vehicle := api.NewMockVehicle(ctrl)
	vehicle.EXPECT().Capacity().Return(0.0).AnyTimes()

	ce := NewEstimator(util.NewLogger("test"), api.NewMockCharger(ctrl), vehicle)

	// neither the passed-in gradient nor the estimator's own is usable;
	// without a guard, 500/0 would produce +Inf, and min(+Inf, 100) would
	// silently clamp to a false 100% instead of surfacing the problem
	ce.Restore(15, 500, 0, 0, true)

	assert.Equal(t, 15.0, ce.State().VehicleSoc, "no usable gradient: seed the anchor instead of dividing by zero")
}
