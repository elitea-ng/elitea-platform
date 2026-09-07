package cutover

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type DecommissionReport struct {
	Ready       bool               `json:"ready"`
	Timestamp   time.Time          `json:"timestamp"`
	TotalEPs    int                `json:"total_endpoints"`
	MigratedEPs int                `json:"migrated_endpoints"`
	Remaining   []EndpointState    `json:"remaining,omitempty"`
	Steps       []DecommissionStep `json:"steps"`
}

type DecommissionStep struct {
	Order       int    `json:"order"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	Manual      bool   `json:"manual"`
}

func GenerateDecommissionReport(ctx context.Context, tracker *Tracker) (DecommissionReport, error) {
	states, err := tracker.List(ctx)
	if err != nil {
		return DecommissionReport{}, err
	}

	report := DecommissionReport{
		Timestamp: time.Now().UTC(),
		TotalEPs:  len(states),
	}

	for _, s := range states {
		if s.Backend == StateGo {
			report.MigratedEPs++
		} else {
			report.Remaining = append(report.Remaining, s)
		}
	}

	report.Ready = report.MigratedEPs == report.TotalEPs && report.TotalEPs > 0

	report.Steps = []DecommissionStep{
		{Order: 1, Description: "Verify all endpoints in 'go' state", Command: "cutover-ctl decommission-check"},
		{Order: 2, Description: "Remove LEGACY_URL from elitea-main config", Command: "kubectl set env deployment/elitea-main LEGACY_URL-"},
		{Order: 3, Description: "Restart elitea-main to drop cutover proxy", Command: "kubectl rollout restart deployment/elitea-main"},
		{Order: 4, Description: "Monitor error rates for 1 hour", Manual: true},
		{Order: 5, Description: "Scale pylon-main to 0 replicas", Command: "kubectl scale deployment/pylon-main --replicas=0"},
		{Order: 6, Description: "Monitor for 24 hours at zero replicas", Manual: true},
		{Order: 7, Description: "Delete pylon-main deployment", Command: "kubectl delete deployment pylon-main"},
		{Order: 8, Description: "Clean up Redis cutover state", Command: "redis-cli DEL elitea:cutover:endpoints"},
		{Order: 9, Description: "Remove shadow config from elitea-main", Command: "kubectl set env deployment/elitea-main SHADOW_ENABLED- SHADOW_LEGACY_URL- SHADOW_WEIGHT-"},
		{Order: 10, Description: "Archive pylon_main codebase", Manual: true},
	}

	return report, nil
}

func (h *AdminHandler) DecommissionReport(w http.ResponseWriter, r *http.Request) {
	report, err := GenerateDecommissionReport(r.Context(), h.tracker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if report.Ready {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusPreconditionFailed)
	}
	_ = json.NewEncoder(w).Encode(report)
}
