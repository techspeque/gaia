package service

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// systemdQuote quotes one word for ExecStart= or Environment=: in double
// quotes, with backslash and quote escaped, and $ and % doubled so systemd
// expands neither.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func renderSystemd(s Spec) []byte {
	var b bytes.Buffer
	b.WriteString("# " + Marker + "\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Gaia " + role(s) + "\n")
	b.WriteString("Documentation=https://github.com/techspeque/gaia\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	words := make([]string, len(s.Args))
	for i, a := range s.Args {
		words[i] = systemdQuote(a)
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	for _, e := range s.Env {
		b.WriteString("Environment=" + systemdQuote(e.Name+"="+e.Value) + "\n")
	}
	for _, n := range s.Unset {
		b.WriteString("UnsetEnvironment=" + n + "\n")
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.Bytes()
}

// role is the daemon role in s's arguments, for the description.
func role(s Spec) string {
	if len(s.Args) > 1 {
		return s.Args[1]
	}
	return "daemon"
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func renderLaunchd(s Spec, log string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + Marker + " -->\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }
	str := func(indent, v string) { b.WriteString(indent + "<string>" + xmlText(v) + "</string>\n") }
	key("Label")
	str("\t", LaunchdLabel)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range s.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(s.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, e := range s.Env {
			b.WriteString("\t\t<key>" + xmlText(e.Name) + "</key>\n")
			str("\t\t", e.Value)
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key("ProcessType")
	str("\t", "Background")
	key("StandardOutPath")
	str("\t", log)
	key("StandardErrorPath")
	str("\t", log)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}
