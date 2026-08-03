package server

import (
	"errors"
	"net/http"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/gorilla/mux"
)

// sessionSplitter is implemented by loadpoints supporting live session splits.
// Asserted dynamically instead of extending loadpoint.API, which would force a
// regeneration of the generated mock in core/loadpoint/mock.go. GetChargedEnergy
// is included for the same reason: it exists on *Loadpoint (core/loadpoint_api.go)
// but is not part of the loadpoint.API interface either.
type sessionSplitter interface {
	SplitSession(api.Vehicle, bool)
	GetChargedEnergy() float64
}

// sessionSplitVehicleNone is the reserved {name} path segment that splits the
// session and detaches the vehicle (guest car). It only takes effect when no
// vehicle of that name exists, so a real vehicle named "none" still wins.
const sessionSplitVehicleNone = "none"

// resolveSplitVehicle resolves the optional {name} path segment of a session
// split into either a vehicle to assign, or the request to detach the vehicle.
// The registry is asked first so a real vehicle can never be shadowed by the
// reserved name.
func resolveSplitVehicle(vehicles site.Vehicles, name string) (api.Vehicle, bool, error) {
	if name == "" {
		return nil, false, nil
	}

	vv, err := vehicles.ByName(name)
	if err == nil {
		return vv.Instance(), false, nil
	}

	if name == sessionSplitVehicleNone {
		return nil, true, nil
	}

	return nil, false, err
}

// sessionSplitHandler ends the running session and starts a new one, optionally
// assigning a different vehicle or detaching the current one
func sessionSplitHandler(site site.API, lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		splitter, ok := lp.(sessionSplitter)
		if !ok {
			jsonError(w, http.StatusNotImplemented, errors.New("session split not supported"))
			return
		}

		// preconditions are checked through lock-protected api methods so we
		// never touch lp.session from this goroutine
		if lp.GetStatus() == api.StatusA {
			jsonError(w, http.StatusConflict, errors.New("no vehicle connected"))
			return
		}

		if splitter.GetChargedEnergy() == 0 {
			jsonError(w, http.StatusConflict, errors.New("session has no energy"))
			return
		}

		v, detach, err := resolveSplitVehicle(site.Vehicles(), mux.Vars(r)["name"])
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		splitter.SplitSession(v, detach)

		res := struct {
			Vehicle string `json:"vehicle"`
			Detach  bool   `json:"detach,omitempty"`
		}{
			Detach: detach,
		}
		if v != nil {
			res.Vehicle = v.GetTitle()
		}

		jsonWrite(w, res)
	}
}
