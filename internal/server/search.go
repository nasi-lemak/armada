package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

// Quick search finds devices (by name, serial, asset tag, IP, hostname,
// location, label or ID), releases and rollouts (fleet-wide accounts), and
// users (admins), among what the caller can see.

const searchLimit = 8

type searchHit struct {
	api.SearchResult
	rank int // lower is better: exact, prefix, then anywhere
}

// matchRank scores how well q (lower case) matches any of the fields.
func matchRank(q string, fields ...string) (int, string) {
	best, why := 99, ""
	for _, f := range fields {
		lf := strings.ToLower(f)
		switch {
		case lf == "":
			continue
		case lf == q:
			return 0, f
		case strings.HasPrefix(lf, q) && best > 1:
			best, why = 1, f
		case strings.Contains(lf, q) && best > 2:
			best, why = 2, f
		}
	}
	return best, why
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []api.SearchResult{}
	if len(q) < 2 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	add := func(hits []searchHit) {
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].rank != hits[j].rank {
				return hits[i].rank < hits[j].rank
			}
			return hits[i].Title < hits[j].Title
		})
		for i, h := range hits {
			if i == searchLimit {
				break
			}
			out = append(out, h.SearchResult)
		}
	}

	devs, err := s.store.ListAllDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var hits []searchHit
	for _, d := range devs {
		if !p.sees(d) {
			continue
		}
		fields := []string{d.Name, d.ID, d.Info.Serial, d.Info.Hostname, d.Info.Model}
		fields = append(fields, d.Info.IPs...)
		if inv := d.Inventory; inv != nil {
			fields = append(fields, inv.AssetTag, inv.Location)
		}
		for k, v := range d.Labels {
			fields = append(fields, k+"="+v)
		}
		rank, why := matchRank(q, fields...)
		if rank == 99 {
			continue
		}
		sub := d.Lifecycle
		if why != d.Name {
			sub = why + " · " + sub
		}
		if d.Lifecycle == api.LifecycleRetired {
			rank += 10 // retired devices last
		}
		hits = append(hits, searchHit{api.SearchResult{Kind: "device", ID: d.ID, Title: d.Name, Subtitle: sub, Link: "#/devices/" + d.ID}, rank})
	}
	add(hits)

	if !p.scoped() {
		rels, err := s.store.ListReleases(ctx, "")
		if err != nil {
			s.internalError(w, err)
			return
		}
		hits = nil
		for _, rel := range rels {
			if rank, _ := matchRank(q, rel.Package+" "+rel.Version, rel.Package, rel.Version, rel.ID); rank < 99 {
				hits = append(hits, searchHit{api.SearchResult{Kind: "release", ID: rel.ID, Title: rel.Package + " " + rel.Version,
					Subtitle: rel.OS + " " + rel.Arch, Link: "#/releases"}, rank})
			}
		}
		add(hits)
		deps, err := s.store.ListDeployments(ctx)
		if err != nil {
			s.internalError(w, err)
			return
		}
		hits = nil
		for _, d := range deps {
			if rank, _ := matchRank(q, d.Name, d.ID, d.Package+" "+d.Version); rank < 99 {
				hits = append(hits, searchHit{api.SearchResult{Kind: "rollout", ID: d.ID, Title: d.Name,
					Subtitle: d.Package + " " + d.Version + " · " + d.Status, Link: "#/deployments/" + d.ID}, rank})
			}
		}
		add(hits)
	}
	if p.fleetRole() == api.RoleAdmin {
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			s.internalError(w, err)
			return
		}
		hits = nil
		for _, u := range users {
			if rank, _ := matchRank(q, u.Email, u.Name); rank < 99 {
				hits = append(hits, searchHit{api.SearchResult{Kind: "user", ID: u.ID, Title: u.Email, Subtitle: u.Role, Link: "#/users"}, rank})
			}
		}
		add(hits)
	}
	writeJSON(w, http.StatusOK, out)
}
