package server

import (
	"errors"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// splitTestVehicles is a minimal site.Vehicles for name resolution tests
type splitTestVehicles struct {
	byName map[string]vehicle.API
}

func (vv *splitTestVehicles) Settings() []vehicle.API { return nil }

func (vv *splitTestVehicles) Instances() []api.Vehicle { return nil }

func (vv *splitTestVehicles) ByName(name string) (vehicle.API, error) {
	if v, ok := vv.byName[name]; ok {
		return v, nil
	}
	return nil, errors.New("vehicle not found: " + name)
}

// TestResolveSplitVehicle covers the three meanings of the session split
// endpoint, including that the reserved detach name never shadows a real
// vehicle of the same name.
func TestResolveSplitVehicle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	instance := api.NewMockVehicle(ctrl)

	adapter := vehicle.NewMockAPI(ctrl)
	adapter.EXPECT().Instance().Return(instance).AnyTimes()

	t.Run("no name keeps the vehicle", func(t *testing.T) {
		vv := &splitTestVehicles{}

		v, detach, err := resolveSplitVehicle(vv, "")
		require.NoError(t, err)
		assert.Nil(t, v)
		assert.False(t, detach)
	})

	t.Run("known name assigns the vehicle", func(t *testing.T) {
		vv := &splitTestVehicles{byName: map[string]vehicle.API{"db:25": adapter}}

		v, detach, err := resolveSplitVehicle(vv, "db:25")
		require.NoError(t, err)
		assert.Equal(t, instance, v)
		assert.False(t, detach)
	})

	t.Run("reserved name detaches the vehicle", func(t *testing.T) {
		vv := &splitTestVehicles{byName: map[string]vehicle.API{"db:25": adapter}}

		v, detach, err := resolveSplitVehicle(vv, "none")
		require.NoError(t, err)
		assert.Nil(t, v)
		assert.True(t, detach)
	})

	t.Run("a real vehicle named none wins over the reserved name", func(t *testing.T) {
		vv := &splitTestVehicles{byName: map[string]vehicle.API{"none": adapter}}

		v, detach, err := resolveSplitVehicle(vv, "none")
		require.NoError(t, err)
		assert.Equal(t, instance, v)
		assert.False(t, detach, "an existing vehicle must not be interpreted as detach")
	})

	t.Run("unknown name is an error", func(t *testing.T) {
		vv := &splitTestVehicles{}

		_, _, err := resolveSplitVehicle(vv, "db:99")
		require.Error(t, err)
	})
}
