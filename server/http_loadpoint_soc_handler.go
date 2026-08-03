package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/gorilla/mux"
)

// socEstimator is implemented by loadpoints exposing their soc estimate.
// Asserted dynamically instead of extending loadpoint.API, which would force
// a regeneration of the generated mock in core/loadpoint/mock.go - same
// reasoning as sessionSplitter in http_loadpoint_handler.go.
type socEstimator interface {
	GetSocEstimate() (soc.State, core.SocEstimate, bool)
	SetSocEstimate(float64) error
	ShiftSocEstimate(float64) error
	ClearSocEstimate() error
}

type socEstimateResponse struct {
	Soc       float64          `json:"soc"`
	Estimator soc.State        `json:"estimator"`
	Persisted core.SocEstimate `json:"persisted"`
	Offset    float64          `json:"offset"`
	AgeHours  float64          `json:"ageHours"`
}

// socEstimateHandler returns the full estimator state
func socEstimateHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		est, ok := lp.(socEstimator)
		if !ok {
			jsonError(w, http.StatusNotImplemented, errors.New("soc estimate not supported"))
			return
		}

		st, se, active := est.GetSocEstimate()
		if !active {
			jsonError(w, http.StatusConflict, core.ErrNoSocEstimator)
			return
		}

		res := socEstimateResponse{
			Soc:       st.VehicleSoc,
			Estimator: st,
			Persisted: se,
			Offset:    st.VehicleSoc - st.FetchedSoc,
		}
		if !se.Updated.IsZero() {
			res.AgeHours = time.Since(se.Updated).Hours()
		}

		jsonWrite(w, res)
	}
}

// socEstimateSetHandler overrides the estimated soc
func socEstimateSetHandler(lp loadpoint.API) http.HandlerFunc {
	return socEstimateWriteHandler(lp, func(est socEstimator, v float64) error {
		return est.SetSocEstimate(v)
	})
}

// socEstimateShiftHandler moves the energy anchor by the given kWh
func socEstimateShiftHandler(lp loadpoint.API) http.HandlerFunc {
	return socEstimateWriteHandler(lp, func(est socEstimator, v float64) error {
		return est.ShiftSocEstimate(v)
	})
}

// socEstimateWriteHandler is the shared implementation behind
// socEstimateSetHandler and socEstimateShiftHandler: parse the {value} path
// var, dispatch to fun, and map the domain error (if any) onto a status
// code.
func socEstimateWriteHandler(lp loadpoint.API, fun func(socEstimator, float64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		est, ok := lp.(socEstimator)
		if !ok {
			jsonError(w, http.StatusNotImplemented, errors.New("soc estimate not supported"))
			return
		}

		v, err := strconv.ParseFloat(mux.Vars(r)["value"], 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		if err := fun(est, v); err != nil {
			if errors.Is(err, core.ErrNoSocEstimator) {
				jsonError(w, http.StatusConflict, err)
			} else {
				jsonError(w, http.StatusBadRequest, err)
			}
			return
		}

		jsonWrite(w, v)
	}
}

// socEstimateClearHandler drops the persisted record
func socEstimateClearHandler(lp loadpoint.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		est, ok := lp.(socEstimator)
		if !ok {
			jsonError(w, http.StatusNotImplemented, errors.New("soc estimate not supported"))
			return
		}

		if err := est.ClearSocEstimate(); err != nil {
			jsonError(w, http.StatusConflict, err)
			return
		}

		jsonWrite(w, true)
	}
}

// vehicleSocEstimateHandler returns a vehicle's persisted estimate, also for
// vehicles that are not currently connected
func vehicleSocEstimateHandler(site site.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]

		se, ok := core.LoadSocEstimate(name)
		if !ok {
			jsonError(w, http.StatusNotFound, errors.New("no estimate for vehicle "+name))
			return
		}

		jsonWrite(w, struct {
			Soc float64 `json:"soc"`
			core.SocEstimate
		}{
			Soc:         se.Soc(),
			SocEstimate: se,
		})
	}
}
