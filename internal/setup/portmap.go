// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"strconv"
	"strings"
)

// Every published port is a mapping: the port the service listens on here,
// and the port the hostname answers on out there.
//
// They are usually the same number and were originally assumed to be, which
// is wrong in both directions. A service already running on 3000 should not
// have to move to be published on 8080; and two deployments publishing
// different services on one well-known public port — 443, 5432 — is the
// ordinary case rather than the exotic one.
//
// Written local:published, in that order, because the local half is the one
// the person setting this up already knows.
type PortMapping struct {
	Local     int32
	Published int32
}

func (m PortMapping) String() string {
	if m.Local == m.Published {
		return strconv.Itoa(int(m.Local))
	}
	return fmt.Sprintf("%d:%d", m.Local, m.Published)
}

// ParsePortMapping accepts "3000" and "3000:8080". A bare number publishes
// the port under its own name, which is what almost everyone wants and what
// every earlier version of this did.
func ParsePortMapping(text string) (PortMapping, error) {
	text = strings.TrimSpace(text)
	local, published, mapped := strings.Cut(text, ":")
	if !mapped {
		published = local
	}

	from, err := parsePort(local)
	if err != nil {
		return PortMapping{}, fmt.Errorf("%q is not a port mapping: %w", text, err)
	}
	to, err := parsePort(published)
	if err != nil {
		return PortMapping{}, fmt.Errorf("%q is not a port mapping: %w", text, err)
	}
	return PortMapping{Local: from, Published: to}, nil
}

func parsePort(text string) (int32, error) {
	port, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q is not a port number between 1 and 65535", strings.TrimSpace(text))
	}
	return int32(port), nil
}

// ParsePortMappings reads the list as typed: "5432, 3000:8080". Empty entries
// are dropped rather than rejected, so a trailing comma is not a reason to
// stop an install.
func ParsePortMappings(list string) ([]PortMapping, error) {
	var mappings []PortMapping
	for _, field := range strings.Split(list, ",") {
		if strings.TrimSpace(field) == "" {
			continue
		}
		mapping, err := ParsePortMapping(field)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

// TcpMappings is the exposed ports as mappings, skipping anything unparseable
// — Validate is what reports those, and reporting them twice in different
// words helps nobody.
func (s Settings) TcpMappings() []PortMapping {
	mappings := make([]PortMapping, 0, len(s.TcpPorts))
	for _, raw := range s.TcpPorts {
		if mapping, err := ParsePortMapping(raw); err == nil {
			mappings = append(mappings, mapping)
		}
	}
	return mappings
}

// ServiceMapping is the TLS-terminated service: the local port it listens on
// and the port the hostname answers HTTPS on.
func (s Settings) ServiceMapping() PortMapping {
	mapping := PortMapping{Local: s.Port(), Published: 443}
	if s.PublishedPort != "" {
		if port, err := parsePort(s.PublishedPort); err == nil {
			mapping.Published = port
		}
	}
	return mapping
}
