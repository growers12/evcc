package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/gorilla/mux"
)

type PlanResponse struct {
	PlanId   int       `json:"planId"`
	PlanTime time.Time `json:"planTime"`
	Duration int64     `json:"duration"`
	Plan     api.Rates `json:"plan"`
	Power    float64   `json:"power"`
}

type PlanPreviewResponse struct {
	PlanTime time.Time `json:"planTime"`
	Duration int64     `json:"duration"`
	Plan     api.Rates `json:"plan"`
	Power    float64   `json:"power"`
}

// planHandler returns the current plan
func planHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		maxPower := lp.EffectiveMaxPower()
		planTime := lp.EffectivePlanTime()
		id := lp.EffectivePlanId()

		goal, _ := lp.GetPlanGoal()
		requiredDuration := lp.GetPlanRequiredDuration(goal, maxPower)
		strategy := lp.EffectivePlanStrategy()
		plan := lp.GetPlan(planTime, requiredDuration, strategy.Precondition, strategy.Continuous)

		res := PlanResponse{
			PlanId:   id,
			PlanTime: planTime,
			Duration: int64(requiredDuration.Seconds()),
			Plan:     plan,
			Power:    maxPower,
		}

		jsonWrite(w, res)
	}
}

// staticPlanPreviewHandler returns a plan preview for given parameters
func staticPlanPreviewHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		planTime, err := time.ParseInLocation(time.RFC3339, vars["time"], nil)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		goal, err := strconv.ParseFloat(vars["value"], 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		switch typ := vars["type"]; typ {
		case "soc":
			if !lp.SocBasedPlanning() {
				jsonError(w, http.StatusBadRequest, errors.New("soc planning only available for vehicles with known soc and capacity"))
				return
			}
		case "energy":
			if lp.SocBasedPlanning() {
				jsonError(w, http.StatusBadRequest, errors.New("energy planning not available for vehicles with known soc and capacity"))
				return
			}
		default:
			jsonError(w, http.StatusBadRequest, fmt.Errorf("invalid plan type: %s", typ))
			return
		}

		maxPower := lp.EffectiveMaxPower()
		requiredDuration := lp.GetPlanRequiredDuration(goal, maxPower)
		strategy := lp.EffectivePlanStrategy()

		plan := lp.GetPlan(planTime, requiredDuration, strategy.Precondition, strategy.Continuous)

		res := PlanPreviewResponse{
			PlanTime: planTime,
			Duration: int64(requiredDuration.Seconds()),
			Plan:     plan,
			Power:    maxPower,
		}

		jsonWrite(w, res)
	}
}

// planEnergyHandler updates plan energy and time
func planEnergyHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		ts, err := time.ParseInLocation(time.RFC3339, vars["time"], nil)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		val, err := strconv.ParseFloat(vars["value"], 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		if err := lp.SetPlanEnergy(ts, val); err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		ts, energy := lp.GetPlanEnergy()

		res := struct {
			Energy float64   `json:"energy"`
			Time   time.Time `json:"time"`
		}{
			Energy: energy,
			Time:   ts,
		}

		jsonWrite(w, res)
	}
}

// planRemoveHandler removes plan time
func planRemoveHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := lp.SetPlanEnergy(time.Time{}, 0); err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		jsonWrite(w, struct{}{})
	}
}

// vehicleSelectHandler sets active vehicle
func vehicleSelectHandler(site site.API, lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		vv, err := site.Vehicles().ByName(vars["name"])
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		v := vv.Instance()
		lp.SetVehicle(v)

		res := struct {
			Vehicle string `json:"vehicle"`
		}{
			Vehicle: v.GetTitle(),
		}

		jsonWrite(w, res)
	}
}

// vehicleRemoveHandler removes vehicle
func vehicleRemoveHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lp.SetVehicle(nil)
		jsonWrite(w, struct{}{})
	}
}

// vehicleDetectHandler starts vehicle detection
func vehicleDetectHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lp.StartVehicleDetection()
		jsonWrite(w, struct{}{})
	}
}

// planStrategyHandler updates plan strategy for loadpoint
func planStrategyHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := planStrategyHandlerSetter(r, lp.SetPlanStrategy); err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		res := lp.GetPlanStrategy()

		jsonWrite(w, res)
	}
}

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
