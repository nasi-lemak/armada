package server

import (
	"fmt"
	"net/http"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Printable asset labels: for each device, a QR code that opens its page
// in the dashboard, with its name, serial number and asset tag. The
// dashboard lays them out for a sheet printer or a label printer.

const maxLabels = 500

// qrPath encodes text as a QR code and returns it as an SVG path of
// horizontal runs, and the grid size.
func qrPath(text string) (string, int, error) {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", 0, err
	}
	q.DisableBorder = true
	bm := q.Bitmap()
	var b strings.Builder
	for y, row := range bm {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start, y, x-start, x-start)
		}
	}
	return b.String(), len(bm), nil
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var ids []string
	for _, v := range r.URL.Query()["ids"] {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "say which devices (ids=dev_a,dev_b)")
		return
	}
	if len(ids) > maxLabels {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d labels at a time", maxLabels))
		return
	}
	base := s.publicURL(r)
	out := []api.AssetLabel{}
	for _, id := range ids {
		d, err := s.store.GetDevice(ctx, id)
		if err == store.ErrNotFound || (err == nil && !p.sees(d)) {
			continue // not yours: no label, same as missing
		} else if err != nil {
			s.internalError(w, err)
			return
		}
		l := api.AssetLabel{DeviceID: d.ID, Name: d.Name, Serial: d.Info.Serial, URL: base + "/#/devices/" + d.ID}
		if d.Inventory != nil {
			l.AssetTag, l.Location = d.Inventory.AssetTag, d.Inventory.Location
		}
		if l.QR, l.QRSize, err = qrPath(l.URL); err != nil {
			s.internalError(w, err)
			return
		}
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, out)
}
