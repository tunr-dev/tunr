package connector

import (
	"errors"
	"strings"
	"text/template"
)

// service.go — service units for `tunr connect install` (P2-T3). Separate
// from `tunr service` (which runs `tunr share`): different name, so a machine
// can run both.

// ServiceName is the systemd unit name and the launchd label suffix.
const (
	SystemdUnitPath = "/etc/systemd/system/tunr-connector.service"
	LaunchdLabel    = "sh.tunr.connector"
)

// ServiceParams are the values rendered into a unit.
type ServiceParams struct {
	BinaryPath string
	ConfigPath string
	User       string // systemd only; "" = root
	Home       string // launchd log location
}

func (p ServiceParams) check() error {
	for _, v := range []string{p.BinaryPath, p.ConfigPath, p.User, p.Home} {
		// Units are line-oriented; a newline or quote in a path could inject
		// directives. Paths with those characters are refused outright.
		if strings.ContainsAny(v, "\n\r\"'<>&") {
			return errors.New("service paths must not contain newlines, quotes or XML metacharacters")
		}
	}
	if p.BinaryPath == "" || p.ConfigPath == "" {
		return errors.New("binary and config paths are required")
	}
	return nil
}

// The connector needs outbound network and read access to its config and
// secrets, nothing else — so the unit takes most of the filesystem away.
const systemdUnit = `[Unit]
Description=tunr connector (Agent Bridge site connector)
Documentation=https://tunr.sh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
{{- if .User}}
User={{.User}}
{{- end}}
ExecStart="{{.BinaryPath}}" connect --config "{{.ConfigPath}}"
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
`

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + LaunchdLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>connect</string>
        <string>--config</string>
        <string>{{.ConfigPath}}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.Home}}/Library/Logs/tunr-connector.log</string>
    <key>StandardErrorPath</key>
    <string>{{.Home}}/Library/Logs/tunr-connector.err</string>
</dict>
</plist>
`

// SystemdUnit renders the systemd unit.
func SystemdUnit(p ServiceParams) (string, error) { return render(systemdUnit, p) }

// LaunchdPlist renders the launchd agent plist.
func LaunchdPlist(p ServiceParams) (string, error) { return render(launchdPlist, p) }

func render(tmpl string, p ServiceParams) (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	var b strings.Builder
	if err := template.Must(template.New("unit").Parse(tmpl)).Execute(&b, p); err != nil {
		return "", err
	}
	return b.String(), nil
}
