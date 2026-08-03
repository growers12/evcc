package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
