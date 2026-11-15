package server

import (
	"net/http"
	"sort"

	"github.com/nasi-lemak/armada/internal/api"
)

// handleGroupings lists the customers, projects (or whatever -group-labels
// names) among the devices the caller can see, for the dashboard's picker.
// It ignores the current view, so the picker can always switch.
func (s *Server) handleGroupings(w http.ResponseWriter, r *http.Request) {
	devs, err := s.store.ListDevices(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	counts := map[string]map[string]int{}
	for _, k := range s.cfg.GroupLabels {
		counts[k] = map[string]int{}
	}
	for _, d := range devs {
		if !p.sees(d) || d.ParentID != "" {
			continue
		}
		for _, k := range s.cfg.GroupLabels {
			if v, ok := d.Labels[k]; ok && v != "" {
				counts[k][v]++
			}
		}
	}
	out := []api.Grouping{}
	for _, k := range s.cfg.GroupLabels {
		g := api.Grouping{Key: k, Values: []api.GroupingValue{}}
		for v, n := range counts[k] {
			g.Values = append(g.Values, api.GroupingValue{Value: v, Devices: n})
		}
		sort.Slice(g.Values, func(i, j int) bool { return g.Values[i].Value < g.Values[j].Value })
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}
