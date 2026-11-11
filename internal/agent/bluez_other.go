//go:build !linux

package agent

import (
	"context"

	"github.com/nasi-lemak/armada/internal/api"
)

func bluezDiscover(ctx context.Context, d DiscoveryConfig) ([]api.PeripheralReport, error) {
	return nil, errNoBluez
}
