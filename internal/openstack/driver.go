// Package openstack is the FelCloud sandbox driver: one small VM per team, on a Neutron
// port that carries the team's IPAM address. It implements lifecycle.Sandboxes.
package openstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	gopenstack "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// Tags put on every team port (GC queries). The name stays the identity.
var portTags = []string{"cstam", "cstam-team"}

// Config of the driver. The IDs come from `pulumi stack output` in infra/pulumi.
type Config struct {
	Cloud          string // clouds.yaml entry; empty uses OS_CLOUD
	NetworkID      string // networkId
	SubnetID       string // subnetId
	TeamSecGroupID string // teamSecGroupId
	Flavor         string // default G0.basic.1c1g
	Image          string // default "Debian 12 - Bookworm"
	// PollInterval between status checks while waiting for Nova (default 2 s).
	PollInterval time.Duration
}

// Driver talks to Nova and Neutron with gophercloud.
type Driver struct {
	cfg      Config
	net      *gophercloud.ServiceClient
	compute  *gophercloud.ServiceClient
	flavorID string
	imageID  string
	sem      chan struct{} // bounds the API calls in flight
}

var _ lifecycle.Sandboxes = (*Driver)(nil)

// New authenticates with clouds.yaml and resolves the flavor and image names once.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.NetworkID == "" || cfg.SubnetID == "" || cfg.TeamSecGroupID == "" {
		return nil, errors.New("openstack: network, subnet and team security group IDs are required")
	}
	if cfg.Cloud == "" {
		cfg.Cloud = os.Getenv("OS_CLOUD")
	}
	if cfg.Flavor == "" {
		cfg.Flavor = "G0.basic.1c1g"
	}
	if cfg.Image == "" {
		cfg.Image = "Debian 12 - Bookworm"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	ao, eo, tlsCfg, err := clouds.Parse(clouds.WithCloudName(cfg.Cloud))
	if err != nil {
		return nil, fmt.Errorf("openstack: clouds.yaml: %w", err)
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pc, err := config.NewProviderClient(actx, ao, config.WithTLSConfig(tlsCfg))
	if err != nil {
		return nil, fmt.Errorf("openstack: authenticate: %w", err)
	}
	d := &Driver{cfg: cfg, sem: make(chan struct{}, 8)}
	if d.net, err = gopenstack.NewNetworkV2(pc, eo); err != nil {
		return nil, fmt.Errorf("openstack: network client: %w", err)
	}
	if d.compute, err = gopenstack.NewComputeV2(pc, eo); err != nil {
		return nil, fmt.Errorf("openstack: compute client: %w", err)
	}
	imageClient, err := gopenstack.NewImageV2(pc, eo)
	if err != nil {
		return nil, fmt.Errorf("openstack: image client: %w", err)
	}
	if d.flavorID, err = findFlavor(actx, d.compute, cfg.Flavor); err != nil {
		return nil, err
	}
	if d.imageID, err = findImage(actx, imageClient, cfg.Image); err != nil {
		return nil, err
	}
	return d, nil
}

func findFlavor(ctx context.Context, c *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := flavors.ListDetail(c, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("openstack: list flavors: %w", err)
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", fmt.Errorf("openstack: list flavors: %w", err)
	}
	for _, f := range all {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("openstack: flavor %q not found", name)
}

func findImage(ctx context.Context, c *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := images.List(c, images.ListOpts{Name: name, Status: images.ImageStatusActive}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("openstack: list images: %w", err)
	}
	all, err := images.ExtractImages(pages)
	if err != nil {
		return "", fmt.Errorf("openstack: list images: %w", err)
	}
	var best *images.Image
	for i := range all {
		if best == nil || all[i].CreatedAt.After(best.CreatedAt) {
			best = &all[i]
		}
	}
	if best == nil {
		return "", fmt.Errorf("openstack: image %q not found", name)
	}
	return best.ID, nil
}

// Create makes the team port and VM (or finds them) and waits until the VM is ACTIVE.
func (d *Driver) Create(ctx context.Context, s lifecycle.Spec) (lifecycle.Ref, error) {
	if err := s.Validate(); err != nil {
		return lifecycle.Ref{}, err
	}
	ref := lifecycle.Ref{Name: s.Name()}
	userData, err := UserData(s)
	if err != nil {
		return ref, err
	}
	port, err := d.ensurePort(ctx, s)
	if err != nil {
		return ref, err
	}
	ref.Port = port.ID
	srv, err := d.ensureServer(ctx, s, port.ID, userData)
	if err != nil {
		return ref, err
	}
	ref.Server = srv.ID
	return ref, d.waitActive(ctx, srv.ID)
}

func (d *Driver) ensurePort(ctx context.Context, s lifecycle.Spec) (*ports.Port, error) {
	existing, err := d.portsByName(ctx, s.Name())
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		p := &existing[0]
		if !hasIP(p, s.IP) {
			return nil, fmt.Errorf("openstack: port %s exists with another IP: %w", s.Name(), lifecycle.ErrIPTaken)
		}
		return p, d.tag(ctx, p)
	}
	d.sem <- struct{}{}
	// POST is not idempotent: no retry here. A lost response shows up as 409 on the fixed IP.
	p, err := ports.Create(ctx, d.net, ports.CreateOpts{
		NetworkID:      d.cfg.NetworkID,
		Name:           s.Name(),
		Description:    "cstam team sandbox",
		FixedIPs:       []ports.IP{{SubnetID: d.cfg.SubnetID, IPAddress: s.IP.String()}},
		SecurityGroups: &[]string{d.cfg.TeamSecGroupID},
	}).Extract()
	<-d.sem
	if err != nil {
		if errors.Is(classify(err), lifecycle.ErrIPTaken) {
			// Maybe our own create succeeded and only the response was lost.
			if again, lerr := d.portsByName(ctx, s.Name()); lerr == nil && len(again) > 0 && hasIP(&again[0], s.IP) {
				return &again[0], d.tag(ctx, &again[0])
			}
		}
		return nil, fmt.Errorf("openstack: create port %s: %w", s.Name(), classify(err))
	}
	return p, d.tag(ctx, p)
}

func (d *Driver) tag(ctx context.Context, p *ports.Port) error {
	missing := false
	for _, t := range portTags {
		missing = missing || !slices.Contains(p.Tags, t)
	}
	if !missing {
		return nil
	}
	return d.call(ctx, func(ctx context.Context) error {
		_, err := attributestags.ReplaceAll(ctx, d.net, "ports", p.ID,
			attributestags.ReplaceAllOpts{Tags: portTags}).Extract()
		if err != nil {
			return fmt.Errorf("openstack: tag port %s: %w", p.Name, err)
		}
		return nil
	})
}

func (d *Driver) ensureServer(ctx context.Context, s lifecycle.Spec, portID string, userData []byte) (*servers.Server, error) {
	existing, err := d.serversByName(ctx, s.Name())
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return &existing[0], nil
	}
	d.sem <- struct{}{}
	srv, err := servers.Create(ctx, d.compute, servers.CreateOpts{
		Name:      s.Name(),
		FlavorRef: d.flavorID,
		ImageRef:  d.imageID,
		Networks:  []servers.Network{{Port: portID}},
		UserData:  userData,
		Metadata:  map[string]string{"cstam": "team", "hackathon": s.Hackathon, "team": s.Team},
	}, nil).Extract()
	<-d.sem
	if err != nil {
		return nil, fmt.Errorf("openstack: create server %s: %w", s.Name(), classify(err))
	}
	return srv, nil
}

func (d *Driver) waitActive(ctx context.Context, id string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	for {
		var srv *servers.Server
		err := d.call(ctx, func(ctx context.Context) error {
			var err error
			srv, err = servers.Get(ctx, d.compute, id).Extract()
			return err
		})
		if err != nil {
			return fmt.Errorf("openstack: server %s: %w", id, err)
		}
		switch srv.Status {
		case "ACTIVE":
			return nil
		case "ERROR":
			return fmt.Errorf("openstack: server %s failed: %s", srv.Name, srv.Fault.Message)
		}
		if err := sleep(ctx, d.cfg.PollInterval); err != nil {
			return fmt.Errorf("openstack: server %s still %s: %w", srv.Name, srv.Status, err)
		}
	}
}

// Delete removes the VM, waits until Nova has released the port, then deletes the port.
// It only touches resources named ref.Name.
func (d *Driver) Delete(ctx context.Context, ref lifecycle.Ref) error {
	if !strings.HasPrefix(ref.Name, lifecycle.NamePrefix) {
		return fmt.Errorf("openstack: refusing to delete %q: not ours", ref.Name)
	}
	srvs, err := d.serversByName(ctx, ref.Name)
	if err != nil {
		return err
	}
	for _, srv := range srvs {
		if err := d.call(ctx, func(ctx context.Context) error {
			return servers.Delete(ctx, d.compute, srv.ID).ExtractErr()
		}); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return fmt.Errorf("openstack: delete server %s: %w", srv.ID, err)
		}
	}
	for _, srv := range srvs {
		if err := d.waitGone(ctx, srv.ID); err != nil {
			return err
		}
	}
	prts, err := d.portsByName(ctx, ref.Name)
	if err != nil {
		return err
	}
	for _, p := range prts {
		if err := d.call(ctx, func(ctx context.Context) error {
			return ports.Delete(ctx, d.net, p.ID).ExtractErr()
		}); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return fmt.Errorf("openstack: delete port %s: %w", p.ID, err)
		}
	}
	return nil
}

func (d *Driver) waitGone(ctx context.Context, id string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	for {
		err := d.call(ctx, func(ctx context.Context) error {
			_, err := servers.Get(ctx, d.compute, id).Extract()
			return err
		})
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("openstack: wait for deletion of %s: %w", id, err)
		}
		if err := sleep(ctx, d.cfg.PollInterval); err != nil {
			return fmt.Errorf("openstack: server %s not deleted yet: %w", id, err)
		}
	}
}

// InUse reports whether a Neutron port holds ip on the sandbox subnet.
func (d *Driver) InUse(ctx context.Context, ip netip.Addr) (bool, error) {
	var found []ports.Port
	err := d.call(ctx, func(ctx context.Context) error {
		pages, err := ports.List(d.net, ports.ListOpts{
			NetworkID: d.cfg.NetworkID,
			FixedIPs:  []ports.FixedIPOpts{{IPAddress: ip.String(), SubnetID: d.cfg.SubnetID}},
		}).AllPages(ctx)
		if err != nil {
			return err
		}
		found, err = ports.ExtractPorts(pages)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("openstack: ports with %s: %w", ip, err)
	}
	return len(found) > 0, nil
}

// List returns the sandboxes (VM or port) whose name starts with prefix.
func (d *Driver) List(ctx context.Context, prefix string) ([]lifecycle.Ref, error) {
	if !strings.HasPrefix(prefix, lifecycle.NamePrefix) {
		return nil, fmt.Errorf("openstack: list prefix %q must start with %s", prefix, lifecycle.NamePrefix)
	}
	byName := map[string]*lifecycle.Ref{}
	var order []string
	get := func(name string) *lifecycle.Ref {
		if r, ok := byName[name]; ok {
			return r
		}
		byName[name] = &lifecycle.Ref{Name: name}
		order = append(order, name)
		return byName[name]
	}
	srvs, err := d.servers(ctx, "^"+regexp.QuoteMeta(prefix))
	if err != nil {
		return nil, err
	}
	for _, s := range srvs {
		if strings.HasPrefix(s.Name, prefix) {
			get(s.Name).Server = s.ID
		}
	}
	prts, err := d.ports(ctx, ports.ListOpts{NetworkID: d.cfg.NetworkID, Tags: "cstam-team"})
	if err != nil {
		return nil, err
	}
	for _, p := range prts {
		if strings.HasPrefix(p.Name, prefix) {
			get(p.Name).Port = p.ID
		}
	}
	out := make([]lifecycle.Ref, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

func (d *Driver) serversByName(ctx context.Context, name string) ([]servers.Server, error) {
	all, err := d.servers(ctx, "^"+regexp.QuoteMeta(name)+"$")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, s := range all { // Nova's name filter is a regex: keep exact matches only
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out, nil
}

func (d *Driver) servers(ctx context.Context, nameRE string) ([]servers.Server, error) {
	var out []servers.Server
	err := d.call(ctx, func(ctx context.Context) error {
		pages, err := servers.List(d.compute, servers.ListOpts{Name: nameRE}).AllPages(ctx)
		if err != nil {
			return err
		}
		out, err = servers.ExtractServers(pages)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("openstack: list servers: %w", err)
	}
	return out, nil
}

func (d *Driver) portsByName(ctx context.Context, name string) ([]ports.Port, error) {
	return d.ports(ctx, ports.ListOpts{NetworkID: d.cfg.NetworkID, Name: name})
}

func (d *Driver) ports(ctx context.Context, opts ports.ListOpts) ([]ports.Port, error) {
	var out []ports.Port
	err := d.call(ctx, func(ctx context.Context) error {
		pages, err := ports.List(d.net, opts).AllPages(ctx)
		if err != nil {
			return err
		}
		out, err = ports.ExtractPorts(pages)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("openstack: list ports: %w", err)
	}
	return out, nil
}

// call runs an idempotent API call (GET, DELETE, tag PUT) with the concurrency bound and
// up to 3 attempts on throttling, 5xx and connection errors.
func (d *Driver) call(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for attempt := range 3 {
		d.sem <- struct{}{}
		err = fn(ctx)
		<-d.sem
		if err == nil || !retryable(err) {
			return err
		}
		if serr := sleep(ctx, time.Duration(200<<attempt)*time.Millisecond); serr != nil {
			return errors.Join(err, serr)
		}
	}
	return err
}

func retryable(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &code) {
		switch code.Actual {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// classify maps the Neutron and Nova errors the controller acts on to lifecycle errors.
func classify(err error) error {
	var code gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &code) {
		return err
	}
	body := string(code.Body)
	switch {
	case code.Actual == http.StatusConflict && strings.Contains(body, "IpAddressAlreadyAllocated"):
		return fmt.Errorf("%w: %w", lifecycle.ErrIPTaken, err)
	case strings.Contains(body, "OverQuota") || strings.Contains(strings.ToLower(body), "quota exceeded") ||
		code.Actual == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %w", lifecycle.ErrQuota, err)
	}
	return err
}

func hasIP(p *ports.Port, ip netip.Addr) bool {
	for _, f := range p.FixedIPs {
		if f.IPAddress == ip.String() {
			return true
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
