package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
)

// Insights: for the devices in view (a customer or project, via the
// picker), what's behind on software, which certificates need attention
// and how often devices lose their connection.

const (
	insightExpiringDays = 30
	insightWorstDevices = 10
)

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	rng, err := parseRange(r.URL.Query().Get("range"), 7*24*time.Hour, s.cfg.HistoryRetention)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, err := s.computeInsights(ctx, p, rng)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// computeInsights is the Needs attention view of what p sees, with
// disconnections over the last rng.
func (s *Server) computeInsights(ctx context.Context, p *principal, rng time.Duration) (*api.Insights, error) {
	now := s.now()
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	in := &api.Insights{OfflineNow: []api.InsightDevice{}, Software: []api.SoftwareStatus{}}
	byID := map[string]*api.Device{}
	for _, d := range devs {
		if !p.inView(d) || d.Lifecycle == api.LifecycleRetired {
			continue
		}
		s.decorate(d)
		byID[d.ID] = d
		if d.ParentID != "" || d.Lifecycle != api.LifecycleActive {
			continue // peripherals and stock don't count here
		}
		in.Devices++
		if d.Status == "online" {
			in.Online++
		} else {
			since := d.LastSeenAt
			in.OfflineNow = append(in.OfflineNow, api.InsightDevice{ID: d.ID, Name: d.Name, Since: &since})
		}
	}
	sort.Slice(in.OfflineNow, func(i, j int) bool { return in.OfflineNow[i].Since.Before(*in.OfflineNow[j].Since) })

	if in.Software, err = s.softwareInsights(ctx, byID); err != nil {
		return nil, err
	}
	if in.Certificates, err = s.certInsights(ctx, byID); err != nil {
		return nil, err
	}
	if in.Connectivity, err = s.connectivityInsights(ctx, byID, now.Add(-rng), now); err != nil {
		return nil, err
	}
	return in, nil
}

// softwareInsights compares each device's installed packages with the
// newest release for its channel and platform.
func (s *Server) softwareInsights(ctx context.Context, devs map[string]*api.Device) ([]api.SoftwareStatus, error) {
	rels, err := s.store.ListReleases(ctx, "")
	if err != nil {
		return nil, err
	}
	ups, err := s.store.ListDeviceUpdates(ctx, "", "")
	if err != nil {
		return nil, err
	}
	updating := map[string]map[string]string{} // device -> package -> status
	for _, u := range ups {
		if !api.IsTerminalUpdateStatus(u.Status) {
			if updating[u.DeviceID] == nil {
				updating[u.DeviceID] = map[string]string{}
			}
			updating[u.DeviceID][u.Package] = u.Status
		}
	}
	byPkg := map[string][]*api.Release{}
	for _, rel := range rels {
		byPkg[rel.Package] = append(byPkg[rel.Package], rel)
	}
	out := []api.SoftwareStatus{}
	for pkg, list := range byPkg {
		st := api.SoftwareStatus{Package: pkg, Behind: []api.SoftwareBehind{}}
		for _, rel := range list {
			if st.Latest == "" || rollout.Compare(rel.Version, st.Latest) > 0 {
				st.Latest = rel.Version
			}
		}
		counted := false
		for _, d := range devs {
			installed, ok := d.Packages[pkg]
			if !ok || installed == "" || d.ParentID != "" {
				continue
			}
			latest, latestID := "", ""
			for _, rel := range list {
				if (rel.Channel == "" || rel.Channel == d.Channel) && rollout.PlatformMatches(rel, d) &&
					(latest == "" || rollout.Compare(rel.Version, latest) > 0) {
					latest, latestID = rel.Version, rel.ID
				}
			}
			if latest == "" {
				continue // nothing released for this device's channel or platform
			}
			counted = true
			if rollout.Compare(installed, latest) >= 0 {
				st.UpToDate++
				continue
			}
			st.Behind = append(st.Behind, api.SoftwareBehind{DeviceID: d.ID, Name: d.Name, Channel: d.Channel,
				Installed: installed, Latest: latest, ReleaseID: latestID, Updating: updating[d.ID][pkg]})
		}
		if !counted {
			continue
		}
		sort.Slice(st.Behind, func(i, j int) bool { return st.Behind[i].Name < st.Behind[j].Name })
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Behind) != len(out[j].Behind) {
			return len(out[i].Behind) > len(out[j].Behind)
		}
		return out[i].Package < out[j].Package
	})
	return out, nil
}

// certInsights counts certificates that are expired, expiring soon, or
// stuck in renewal.
func (s *Server) certInsights(ctx context.Context, devs map[string]*api.Device) (api.CertificateInsights, error) {
	ci := api.CertificateInsights{Items: []*api.DeviceCert{}}
	all, err := s.store.ListDeviceCerts(ctx, "")
	if err != nil {
		return ci, err
	}
	now := s.now()
	for _, c := range all {
		d := devs[c.DeviceID]
		if d == nil {
			continue
		}
		ci.Total++
		c.DeviceName = d.Name
		c.DaysLeft = daysLeft(c.Installed, now)
		attention := false
		switch {
		case c.DaysLeft == nil:
		case *c.DaysLeft < 0:
			ci.Expired++
			attention = true
		case *c.DaysLeft <= insightExpiringDays:
			ci.Expiring++
			attention = true
		}
		if c.Renewal != nil {
			switch c.Renewal.Status {
			case "csr_ready":
				ci.Waiting++
				attention = true
			case "failed":
				ci.Failed++
				attention = true
			}
		}
		if attention {
			ci.Items = append(ci.Items, c)
		}
	}
	sort.Slice(ci.Items, func(i, j int) bool {
		a, b := ci.Items[i].DaysLeft, ci.Items[j].DaysLeft
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return *a < *b
	})
	return ci, nil
}

// connectivityInsights summarizes outages that ended in [from, to).
func (s *Server) connectivityInsights(ctx context.Context, devs map[string]*api.Device, from, to time.Time) (api.ConnectivityInsight, error) {
	ci := api.ConnectivityInsight{From: from, To: to, Worst: []api.DeviceOutageStat{}}
	outs, err := s.store.ListOutages(ctx, from, to, "")
	if err != nil {
		return ci, err
	}
	per := map[string]*api.DeviceOutageStat{}
	for _, o := range outs { // newest first
		d := devs[o.DeviceID]
		if d == nil {
			continue
		}
		ci.Outages++
		ci.OfflineSeconds += o.Seconds
		st := per[o.DeviceID]
		if st == nil {
			st = &api.DeviceOutageStat{DeviceID: d.ID, Name: d.Name, LastAt: o.EndedAt, LastCause: o.Cause, LastDetail: o.Detail}
			per[o.DeviceID] = st
		}
		st.Outages++
		st.OfflineSeconds += o.Seconds
		switch o.Cause {
		case api.OutageNetwork:
			ci.Network++
			st.Network++
		case api.OutageRestart:
			ci.Restart++
			st.Restart++
		default:
			ci.Unknown++
		}
	}
	ci.DevicesHit = len(per)
	for _, st := range per {
		ci.Worst = append(ci.Worst, *st)
	}
	sort.Slice(ci.Worst, func(i, j int) bool {
		a, b := ci.Worst[i], ci.Worst[j]
		if a.Outages != b.Outages {
			return a.Outages > b.Outages
		}
		return a.OfflineSeconds > b.OfflineSeconds
	})
	if len(ci.Worst) > insightWorstDevices {
		ci.Worst = ci.Worst[:insightWorstDevices]
	}
	return ci, nil
}

// handleDeviceOutages lists one device's outages.
func (s *Server) handleDeviceOutages(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	rng, err := parseRange(r.URL.Query().Get("range"), 30*24*time.Hour, s.cfg.HistoryRetention)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	outs, err := s.store.ListOutages(r.Context(), now.Add(-rng), now, d.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if outs == nil {
		outs = []*api.Outage{}
	}
	writeJSON(w, http.StatusOK, outs)
}
