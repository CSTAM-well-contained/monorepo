package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/CSTAM-well-contained/monorepo/internal/store"
)

// ErrNoPeer means a planned failover was refused because no other gateway could take the VIP.
var ErrNoPeer = errors.New("no healthy peer gateway")

// Failover asks gateway gw to step down (planned failover): cpd publishes it in ForceBackup,
// its agent writes 1 into keepalived's force-backup file, the instance goes to FAULT and
// the peer takes the VIP. No process is killed, so requests in flight finish.
//
// Unless force is set, it is refused when no other gateway is alive, serving the current
// generation, out of FAULT and not forced down itself: failing over to a stale or sick
// gateway is the outage this button is meant to avoid. Asking twice is not an error.
func (c *Controller) Failover(ctx context.Context, gw string, force bool) error {
	return c.setForceBackup(ctx, gw, true, force)
}

// Failback clears the request of Failover. Because of nopreempt the gateway comes back as
// BACKUP; it takes the VIP again only if the peer fails or is failed over.
func (c *Controller) Failback(ctx context.Context, gw string) error {
	return c.setForceBackup(ctx, gw, false, true)
}

func (c *Controller) setForceBackup(ctx context.Context, gw string, on, force bool) error {
	if !labelRE.MatchString(gw) {
		return fmt.Errorf("%w: bad gateway name", ErrInvalidSpec)
	}
	changed := false
	err := store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		changed = false
		rows, err := tx.Query(ctx,
			`SELECT name, vrrp_state, force_backup,
			   last_seen > now() - $1 * interval '1 millisecond',
			   acked_gen >= (SELECT gen FROM routes_generation)
			 FROM gateways ORDER BY name FOR UPDATE`, GatewayAlive.Milliseconds())
		if err != nil {
			return fmt.Errorf("failover: %w", err)
		}
		type gwRow struct {
			Name, VRRP           string
			Forced, Alive, InSync bool
		}
		gws, err := pgx.CollectRows(rows, pgx.RowToStructByPos[gwRow])
		if err != nil {
			return fmt.Errorf("failover: %w", err)
		}
		found, peerOK := false, false
		for _, g := range gws {
			if g.Name == gw {
				found = true
				changed = g.Forced != on
				continue
			}
			if g.Alive && g.InSync && !g.Forced && g.VRRP != "FAULT" && g.VRRP != "STOP" {
				peerOK = true
			}
		}
		if !found {
			return fmt.Errorf("gateway %s: %w", gw, ErrNotFound)
		}
		if !changed {
			return nil
		}
		if on && !force && !peerOK {
			return fmt.Errorf("%w: every other gateway is silent, behind, in FAULT or forced down (force=true overrides)",
				ErrNoPeer)
		}
		if _, err := tx.Exec(ctx, `UPDATE gateways SET force_backup = $2 WHERE name = $1`, gw, on); err != nil {
			return fmt.Errorf("failover: %w", err)
		}
		// A new generation wakes the long-polls; acks then show who applied it.
		if _, err := tx.Exec(ctx, `UPDATE routes_generation SET gen = gen + 1`); err != nil {
			return fmt.Errorf("failover: %w", err)
		}
		kind := "gateway.failback"
		if on {
			kind = "gateway.failover"
		}
		return c.event(ctx, tx, kind, 0, map[string]any{"gateway": gw, "force": force && on})
	})
	if err == nil && changed {
		c.notify()
	}
	return err
}
