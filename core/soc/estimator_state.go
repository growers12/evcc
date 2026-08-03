package soc

// State is a snapshot of the estimator's internal state. It exists so the
// estimate can be inspected and persisted from outside the package.
type State struct {
	VehicleSoc        float64 `json:"vehicleSoc"`
	FetchedSoc        float64 `json:"fetchedSoc"`
	PrevSoc           float64 `json:"prevSoc"`
	PrevChargedEnergy float64 `json:"prevChargedEnergy"`
	InitialSoc        float64 `json:"initialSoc"`
	InitialEnergy     float64 `json:"initialEnergy"`
	EnergyPerSocStep  float64 `json:"energyPerSocStep"`
	VirtualCapacity   float64 `json:"virtualCapacity"`
	Learned           bool    `json:"learned"`
}

// State returns a snapshot of the estimator's internal state
func (s *Estimator) State() State {
	return State{
		VehicleSoc:        s.vehicleSoc,
		FetchedSoc:        s.fetchedSoc,
		PrevSoc:           s.prevSoc,
		PrevChargedEnergy: s.prevChargedEnergy,
		InitialSoc:        s.initialSoc,
		InitialEnergy:     s.initialEnergy,
		EnergyPerSocStep:  s.energyPerSocStep,
		VirtualCapacity:   s.virtualCapacity,
		Learned:           s.learned,
	}
}
