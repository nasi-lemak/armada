package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// pushHold is how long we ask the server to hold the long-poll. The server
// may hold it for less (its -push-hold), never more.
const pushHold = 50 * time.Second

// pushLoop keeps one long-poll request open so the server can wake this
// device at once: when the request returns with wake=true the agent checks
// in immediately instead of at the next heartbeat.
func (a *Agent) pushLoop(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		wake, err := a.client.wait(ctx, pushHold)
		switch {
		case ctx.Err() != nil:
			return
		case IsStatus(err, http.StatusNotFound):
			a.log.Info("server does not support push; actions run at the next heartbeat")
			return
		case err != nil:
			// Server restarting or network down: heartbeats carry on regardless.
			a.log.Debug("push connection lost", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		backoff = 5 * time.Second
		if wake {
			select {
			case a.wake <- struct{}{}:
			default: // a check-in is already pending
			}
		}
	}
}

// wait performs one long-poll.
func (c *client) wait(ctx context.Context, hold time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hold+20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v1/agent/wait?hold="+strconv.Itoa(int(hold/time.Second)), nil)
	if err != nil {
		return false, err
	}
	if k := c.key(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	req.Header.Set("User-Agent", "armada-agent/"+Version)
	resp, err := c.download.Do(req) // no client timeout; bounded by ctx
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false, readHTTPError(resp)
	}
	var wr api.WaitResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return false, err
	}
	return wr.Wake, nil
}
