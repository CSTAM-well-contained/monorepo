package openstack

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// The team VM runs one app on AppPort: a default page (python3 is in the Debian image),
// or the team's container image. Every value is validated by Spec.Validate before it is
// rendered, so nothing here can break the YAML or the commands.
var userDataTmpl = template.Must(template.New("user-data").Parse(`#cloud-config
{{- if .Image}}
package_update: true
packages: [docker.io]
runcmd:
  - [systemctl, enable, --now, docker]
  - [docker, run, -d, --restart, unless-stopped, --name, app, -p, "{{.AppPort}}:{{.AppPort}}", "{{.Image}}"]
{{- else}}
write_files:
  - path: /srv/cstam/index.html
    content: |
      <!doctype html><title>{{.Team}}</title>
      <h1>team {{.Team}}</h1><p>hackathon {{.Hackathon}}, sandbox {{.IP}}</p>
  - path: /etc/systemd/system/cstam-app.service
    content: |
      [Unit]
      Description=CSTAM default team page
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 -m http.server {{.AppPort}} --directory /srv/cstam
      DynamicUser=yes
      AmbientCapabilities=CAP_NET_BIND_SERVICE
      Restart=always
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, enable, --now, cstam-app.service]
{{- end}}
`))

// UserData renders the cloud-init of a team VM.
func UserData(s lifecycle.Spec) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := userDataTmpl.Execute(&b, s); err != nil {
		return nil, fmt.Errorf("openstack: render user-data: %w", err)
	}
	return b.Bytes(), nil
}
