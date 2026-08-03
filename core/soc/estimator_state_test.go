package soc

import (
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
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
