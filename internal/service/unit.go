package service

import (
	"bytes"
	"encoding/xml"
	"strings"
)

const (
	// Label names the launchd job.
	Label = "io.github.mickamy.pgswitch"
	// UnitName names the systemd user unit.
	UnitName = "pgswitch.service"
)

// Spec is what the service runs.
type Spec struct {
	// Binary is the pgswitch executable.
	Binary string
	// Args follow Binary, starting with serve.
	Args []string
	// LogPath receives stdout and stderr under launchd; systemd keeps them in
	// the journal.
	LogPath string
}

// Plist returns the launchd job for spec: started at login, and again if it
// fails.
func Plist(spec Spec) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, arg := range append([]string{spec.Binary}, spec.Args...) {
		b.WriteString("\t\t<string>" + xmlText(arg) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlText(spec.LogPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(spec.LogPath) + `</string>
</dict>
</plist>
`)

	return b.Bytes()
}

// SystemdUnit returns the systemd user unit for spec: started with the user's
// session, and again if it fails.
func SystemdUnit(spec Spec) []byte {
	words := make([]string, 0, 1+len(spec.Args))
	for _, w := range append([]string{spec.Binary}, spec.Args...) {
		words = append(words, systemdWord(w))
	}

	return []byte(`[Unit]
Description=pgswitch: per-branch Postgres databases

[Service]
ExecStart=` + strings.Join(words, " ") + `
Restart=on-failure

[Install]
WantedBy=default.target
`)
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder does not fail

	return b.String()
}

// systemdWord quotes s for ExecStart: systemd splits on spaces, and expands
// % specifiers and $ variables.
func systemdWord(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if s != "" && !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
