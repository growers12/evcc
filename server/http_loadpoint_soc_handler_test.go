package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// socEstimateTestLoadpoint embeds the generated loadpoint mock so it satisfies
// loadpoint.API, and adds the socEstimator methods the handlers assert on.
type socEstimateTestLoadpoint struct {
	*loadpoint.MockAPI
	active bool

	setCalls   int
	shiftCalls int
	clearCalls int

	setErr   error
	shiftErr error
	clearErr error
}

func (lp *socEstimateTestLoadpoint) GetSocEstimate() (soc.State, core.SocEstimate, bool) {
	return soc.State{VehicleSoc: 20, FetchedSoc: 15}, core.SocEstimate{AnchorSoc: 15}, lp.active
}

func (lp *socEstimateTestLoadpoint) SetSocEstimate(float64) error {
	lp.setCalls++
	if !lp.active {
		return core.ErrNoSocEstimator
	}
	return lp.setErr
}

func (lp *socEstimateTestLoadpoint) ShiftSocEstimate(float64) error {
	lp.shiftCalls++
	if !lp.active {
		return core.ErrNoSocEstimator
	}
	return lp.shiftErr
}

func (lp *socEstimateTestLoadpoint) ClearSocEstimate() error {
	lp.clearCalls++
	if !lp.active {
		return core.ErrNoSocEstimator
	}
	return lp.clearErr
}

func newSocEstimateTestLoadpoint(t *testing.T, active bool) *socEstimateTestLoadpoint {
	t.Helper()
	ctrl := gomock.NewController(t)
	return &socEstimateTestLoadpoint{MockAPI: loadpoint.NewMockAPI(ctrl), active: active}
}

// withValue attaches a mux {value} path var the way the real router would
// after matching e.g. /soc/{value:[0-9.]+}.
func withValue(r *http.Request, value string) *http.Request {
	return mux.SetURLVars(r, map[string]string{"value": value})
}

func TestSocEstimateHandlerWithoutEstimator(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, false)

	w := httptest.NewRecorder()
	socEstimateHandler(lp)(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSocEstimateHandlerReturnsState(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	socEstimateHandler(lp)(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"offset":5`)
}

// TestSocEstimateHandlerNotImplemented covers the loadpoint that does not
// implement the socEstimator interface at all - a bare generated mock.
func TestSocEstimateHandlerNotImplemented(t *testing.T) {
	ctrl := gomock.NewController(t)
	lp := loadpoint.NewMockAPI(ctrl)

	w := httptest.NewRecorder()
	socEstimateHandler(lp)(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestSocEstimateSetHandlerSuccess(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/42", nil), "42")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.setCalls)
	assert.Equal(t, 0, lp.shiftCalls)
}

// TestVehicleSocEstimateHandlerNotFound covers the vehicle endpoint's 404,
// which is distinct from the loadpoint endpoint's 409: 409 means "no
// estimator is active on this loadpoint right now", 404 means "no record
// exists for this vehicle name at all" - the case this endpoint exists for,
// reading a persisted estimate for a vehicle that is not currently connected.
func TestVehicleSocEstimateHandlerNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{"name": "does-not-exist"})

	vehicleSocEstimateHandler()(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestVehicleSocEstimateHandlerReturnsRecord seeds a persisted record through
// the real settings store (the same path saveSocEstimate writes through) and
// checks the handler surfaces both the computed soc and the raw persisted
// fields a dashboard needs.
func TestVehicleSocEstimateHandlerReturnsRecord(t *testing.T) {
	require.NoError(t, settings.SetJson("vehicle.test:http-vehicle.socEstimate", core.SocEstimate{
		AnchorSoc:         15,
		EnergySinceAnchor: 500,
		EnergyPerSocStep:  100,
		Samples:           2,
		Updated:           time.Now(),
	}))

	w := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{"name": "test:http-vehicle"})

	vehicleSocEstimateHandler()(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"soc":20`)
	assert.Contains(t, w.Body.String(), `"anchorSoc":15`)
}

func TestSocEstimateSetHandlerUnparseableValue(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/not-a-number", nil), "not-a-number")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, lp.setCalls, "conversion must fail before the domain method is ever called")
}

func TestSocEstimateSetHandlerOutOfRange(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)
	lp.setErr = errors.New("soc out of range: 150.0")

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/150", nil), "150")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestSocEstimateSetHandlerBelowSource covers the rejection added in
// soc.Estimator.SetSoc: a target below the vehicle's own value would revert
// within one poll, so it has to come back as a 400 naming the floor rather
// than a 200 that quietly does nothing.
func TestSocEstimateSetHandlerBelowSource(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)
	lp.setErr = errors.New("soc estimate cannot be set below the value reported by the vehicle (40.0%): 30.0")

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/30", nil), "30")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "reported by the vehicle", "the operator has to learn what the floor is")
}

func TestSocEstimateSetHandlerNoEstimator(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, false)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/42", nil), "42")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSocEstimateSetHandlerNotImplemented(t *testing.T) {
	ctrl := gomock.NewController(t)
	lp := loadpoint.NewMockAPI(ctrl)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/42", nil), "42")
	socEstimateSetHandler(lp)(w, req)

	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestSocEstimateShiftHandlerSuccess(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/energy/1.5", nil), "1.5")
	socEstimateShiftHandler(lp)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.shiftCalls)
	assert.Equal(t, 0, lp.setCalls)
}

func TestSocEstimateShiftHandlerNegativeValue(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/energy/-2.0", nil), "-2.0")
	socEstimateShiftHandler(lp)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.shiftCalls)
}

func TestSocEstimateShiftHandlerNoEstimator(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, false)

	w := httptest.NewRecorder()
	req := withValue(httptest.NewRequest(http.MethodPost, "/soc/energy/1.5", nil), "1.5")
	socEstimateShiftHandler(lp)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSocEstimateClearHandlerSuccess(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	w := httptest.NewRecorder()
	socEstimateClearHandler(lp)(w, httptest.NewRequest(http.MethodDelete, "/soc", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.clearCalls)
}

func TestSocEstimateClearHandlerNoEstimator(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, false)

	w := httptest.NewRecorder()
	socEstimateClearHandler(lp)(w, httptest.NewRequest(http.MethodDelete, "/soc", nil))

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSocEstimateClearHandlerNotImplemented(t *testing.T) {
	ctrl := gomock.NewController(t)
	lp := loadpoint.NewMockAPI(ctrl)

	w := httptest.NewRecorder()
	socEstimateClearHandler(lp)(w, httptest.NewRequest(http.MethodDelete, "/soc", nil))

	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

// TestSocEstimateRouteOrdering verifies, through an actual gorilla/mux
// router built with the exact patterns registered in http.go, that
// /soc/energy/{value} is not shadowed by /soc/{value}. It posts to
// /soc/energy/1.5 and asserts the shift handler ran (shiftCalls==1), not the
// set handler (setCalls==0) - a body/status assertion alone would not
// distinguish the two, since both handlers respond 200 with the same shape.
func TestSocEstimateRouteOrdering(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	router := mux.NewRouter()
	// registered in the same relative order as server/http.go's route map;
	// map iteration order in http.go is randomized, so this also proves the
	// regexes themselves are sufficient and registration order is not load
	// bearing
	router.Methods(http.MethodPost).Path("/soc/{value:[0-9.]+}").Handler(socEstimateSetHandler(lp))
	router.Methods(http.MethodPost).Path("/soc/energy/{value:-?[0-9.]+}").Handler(socEstimateShiftHandler(lp))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/soc/energy/1.5", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.shiftCalls, "expected the shift handler to run for /soc/energy/1.5")
	assert.Equal(t, 0, lp.setCalls, "the set handler must not have matched /soc/energy/1.5")
}

// TestSocEstimateRouteOrderingSetStillMatches guards the other direction: a
// plain numeric value must still hit the set handler and not accidentally
// fall through to /soc/energy/{value}.
func TestSocEstimateRouteOrderingSetStillMatches(t *testing.T) {
	lp := newSocEstimateTestLoadpoint(t, true)

	router := mux.NewRouter()
	router.Methods(http.MethodPost).Path("/soc/{value:[0-9.]+}").Handler(socEstimateSetHandler(lp))
	router.Methods(http.MethodPost).Path("/soc/energy/{value:-?[0-9.]+}").Handler(socEstimateShiftHandler(lp))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/soc/42", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, lp.setCalls)
	assert.Equal(t, 0, lp.shiftCalls)
}
