package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
)

// Limits of a pushed config.
const (
	MaxExtraLines   = 20
	MaxExtraLineLen = 200
	MaxReasonLen    = 200
)

// ConfigVersion is one HAProxy config version owned by cpd.
type ConfigVersion struct {
	ID         int64     `json:"id"`
	Version    string    `json:"version"` // c<id>, as served in X-Config-Version
	RateLimit  int       `json:"rate_limit"`
	Extra      []string  `json:"extra"`
	Reason     string    `json:"reason"`
	CopiedFrom *int64    `json:"copied_from,omitempty"` // set on a rollback
	CreatedAt  time.Time `json:"created_at"`
	Current    bool      `json:"current"` // the newest: what the gateways should serve
}

// ConfigIn is a new config pushed by an admin.
type ConfigIn struct {
	RateLimit int      `json:"rate_limit"`
	Extra     []string `json:"extra"`
	Reason    string   `json:"reason"`
}

// Validate checks the limits. HAProxy itself checks the lines (haproxy -c) on each gateway,
// and the applier rolls back a config that loads but does not serve.
func (in ConfigIn) Validate() error {
	var errs []error
	if in.RateLimit < 1 || in.RateLimit > 1000000 {
		errs = append(errs, fmt.Errorf("rate_limit %d, want 1 to 1000000", in.RateLimit))
	}
	if len(in.Extra) > MaxExtraLines {
		errs = append(errs, fmt.Errorf("%d extra lines, at most %d", len(in.Extra), MaxExtraLines))
	}
	for i, l := range in.Extra {
		if l == "" || len(l) > MaxExtraLineLen || !printable(l) {
			errs = append(errs, fmt.Errorf("extra line %d: 1 to %d printable ASCII characters", i+1, MaxExtraLineLen))
		}
	}
	if len(in.Reason) > MaxReasonLen || !printable(in.Reason) {
		errs = append(errs, fmt.Errorf("reason: at most %d printable ASCII characters", MaxReasonLen))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return nil
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func versionName(id int64) string { return "c" + strconv.FormatInt(id, 10) }

const configSelect = `SELECT id, rate_limit, extra, reason, copied_from, created_at,
	id = (SELECT max(id) FROM config_versions) FROM config_versions`

func scanConfig(row pgx.CollectableRow) (ConfigVersion, error) {
	var v ConfigVersion
	err := row.Scan(&v.ID, &v.RateLimit, &v.Extra, &v.Reason, &v.CopiedFrom, &v.CreatedAt, &v.Current)
	v.Version = versionName(v.ID)
	if v.Extra == nil {
		v.Extra = []string{}
	}
	return v, err
}

// ConfigVersions lists the config versions, newest first.
func (c *Controller) ConfigVersions(ctx context.Context) ([]ConfigVersion, error) {
	rows, err := c.db.Query(ctx, configSelect+` ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("config versions: %w", err)
	}
	return pgx.CollectRows(rows, scanConfig)
}

// PushConfig stores a new config version and publishes it to the gateways.
func (c *Controller) PushConfig(ctx context.Context, in ConfigIn) (ConfigVersion, error) {
	if err := in.Validate(); err != nil {
		return ConfigVersion{}, err
	}
	return c.insertConfig(ctx, in, nil)
}

// RollbackConfig publishes a copy of version id as a new version (roll forward to old content).
func (c *Controller) RollbackConfig(ctx context.Context, id int64) (ConfigVersion, error) {
	var in ConfigIn
	err := c.db.QueryRow(ctx, `SELECT rate_limit, extra FROM config_versions WHERE id = $1`, id).
		Scan(&in.RateLimit, &in.Extra)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigVersion{}, fmt.Errorf("config version %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return ConfigVersion{}, fmt.Errorf("rollback config: %w", err)
	}
	in.Reason = "rollback to " + versionName(id)
	return c.insertConfig(ctx, in, &id)
}

func (c *Controller) insertConfig(ctx context.Context, in ConfigIn, from *int64) (ConfigVersion, error) {
	if in.Extra == nil {
		in.Extra = []string{}
	}
	var v ConfigVersion
	err := store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO config_versions (rate_limit, extra, reason, copied_from) VALUES ($1, $2, $3, $4) RETURNING id`,
			in.RateLimit, in.Extra, in.Reason, from).Scan(&id); err != nil {
			return fmt.Errorf("push config: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE routes_generation SET gen = gen + 1`); err != nil {
			return fmt.Errorf("push config: %w", err)
		}
		rows, err := tx.Query(ctx, configSelect+` WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("push config: %w", err)
		}
		if v, err = pgx.CollectExactlyOneRow(rows, scanConfig); err != nil {
			return fmt.Errorf("push config: %w", err)
		}
		kind := "config.created"
		if from != nil {
			kind = "config.rollback"
		}
		return c.event(ctx, tx, kind, 0, map[string]any{"version": v.Version, "reason": in.Reason})
	})
	if err != nil {
		return ConfigVersion{}, err
	}
	c.notify()
	return v, nil
}

// currentConfig returns the newest config for the desired state, nil when there is none.
func currentConfig(ctx context.Context, tx pgx.Tx) (*model.Config, error) {
	var id int64
	cfg := &model.Config{}
	err := tx.QueryRow(ctx, `SELECT id, rate_limit, extra FROM config_versions ORDER BY id DESC LIMIT 1`).
		Scan(&id, &cfg.RateLimit, &cfg.Extra)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg.Version = versionName(id)
	return cfg, nil
}

// cleanAckError keeps a gateway's error short and printable before it is stored and shown.
func cleanAckError(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
