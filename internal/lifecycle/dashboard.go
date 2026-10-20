package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// GatewayAlive is how long a gateway counts as alive after its last ack. The agent acks after
// every long-poll answer (at most MaxWait = 25 s apart) and at once on a VRRP change.
const GatewayAlive = 40 * time.Second

// Event is one row of the append-only event log.
type Event struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Kind   string          `json:"kind"`
	Team   string          `json:"team,omitempty"`
	Detail json.RawMessage `json:"detail"`
}

// Address is one address of the team pool and what holds it.
type Address struct {
	IP         string     `json:"ip"`
	State      string     `json:"state"` // free, allocated, quarantine, reserved
	Team       string     `json:"team,omitempty"`
	Hackathon  string     `json:"hackathon,omitempty"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// AllTeams lists the teams of every hackathon that are not deleted, newest first.
func (c *Controller) AllTeams(ctx context.Context) ([]Team, error) {
	rows, err := c.db.Query(ctx, teamSelect+` WHERE t.state <> 'deleted' ORDER BY t.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	return pgx.CollectRows(rows, scanTeam)
}

// Events returns up to limit events with an id above after, oldest first. With after < 0 it
// returns the last limit events.
func (c *Controller) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	limit = min(max(limit, 1), 500)
	q := `SELECT e.id, e.at, e.kind, coalesce(t.name, ''), e.detail
	      FROM events e LEFT JOIN teams t ON t.id = e.team_id
	      WHERE e.id > $1 ORDER BY e.id LIMIT $2`
	if after < 0 {
		q = `SELECT * FROM (SELECT e.id, e.at, e.kind, coalesce(t.name, ''), e.detail
		      FROM events e LEFT JOIN teams t ON t.id = e.team_id
		      WHERE e.id > $1 ORDER BY e.id DESC LIMIT $2) last ORDER BY id`
	}
	rows, err := c.db.Query(ctx, q, after, limit)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	evs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Event])
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	return evs, nil
}

// Addresses returns every address of the team pool in order.
func (c *Controller) Addresses(ctx context.Context) ([]Address, error) {
	rows, err := c.db.Query(ctx,
		`SELECT host(p.ip), p.state, coalesce(t.name, ''), coalesce(h.name, ''), p.released_at, p.updated_at
		 FROM ip_pool p LEFT JOIN teams t ON t.id = p.team_id LEFT JOIN hackathons h ON h.id = t.hackathon_id
		 ORDER BY p.ip`)
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	as, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Address])
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	return as, nil
}

// Routes returns the routing table the gateways should serve now.
func (c *Controller) Routes(ctx context.Context) (model.Desired, error) {
	return c.desired(ctx)
}

// alerts turns the gateway reports into the warnings the dashboard shows. The rules live
// here, not in the browser, so the CLI and the tests see the same thing.
func alerts(gen int64, config string, gws []Gateway) []string {
	out := []string{}
	masters, alive := 0, 0
	for _, g := range gws {
		if !g.Alive {
			out = append(out, fmt.Sprintf("%s: no report since %s", g.Name, g.LastSeen.UTC().Format(time.RFC3339)))
			continue
		}
		alive++
		if g.VRRPState == "MASTER" {
			masters++
		}
		if g.AckedGen < gen {
			out = append(out, fmt.Sprintf("%s is behind: generation %d of %d", g.Name, g.AckedGen, gen))
		}
		if g.VRRPState == "FAULT" {
			out = append(out, g.Name+" is in FAULT (unhealthy or forced backup)")
		}
		if config != "" && g.ConfigTried == config && g.ConfigOutcome != "applied" && g.ConfigOutcome != "" {
			out = append(out, fmt.Sprintf("%s did not take config %s (%s), still serves %s",
				g.Name, config, g.ConfigOutcome, g.ConfigVersion))
		}
	}
	switch {
	case masters > 1:
		out = append(out, "split brain: more than one gateway is MASTER")
	case masters == 0 && alive > 0:
		out = append(out, "no gateway holds the VIP: public traffic is down")
	}
	return out
}
