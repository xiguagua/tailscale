// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// resolvconfIsOpenresolv reports whether the `resolvconf` binary on
// the system is the openresolv implementation.
func resolvconfIsOpenresolv() bool {
	bs, err := exec.Command("resolvconf", "--version").CombinedOutput()
	if err != nil {
		// Either resolvconf isn't installed, or it's not openresolv.
		return false
	}

	return bytes.Contains(bs, []byte("openresolv "))
}

// openresolvManager manages DNS configuration using the openresolv
// implementation of the `resolvconf` program.
type openresolvManager struct{}

func newOpenresolvManager() openresolvManager {
	return openresolvManager{}
}

func (m openresolvManager) SetDNS(config OSConfig) error {
	var stdin bytes.Buffer
	writeResolvConf(&stdin, config.Nameservers, config.SearchDomains)

	cmd := exec.Command("resolvconf", "-m", "0", "-x", "-a", "tailscale")
	cmd.Stdin = &stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running %s: %s", cmd, out)
	}
	return nil
}

func (m openresolvManager) SupportsSplitDNS() bool {
	return false
}

func (m openresolvManager) GetBaseConfig() (OSConfig, error) {
	// List the names of all config snippets openresolv is aware
	// of. Snippets get listed in priority order (most to least),
	// which we'll exploit later.
	bs, err := exec.Command("resolvconf", "-i").CombinedOutput()
	if err != nil {
		return OSConfig{}, err
	}

	// Remove the "tailscale" snippet from the list.
	args := []string{"-l"}
	for _, f := range strings.Split(strings.TrimSpace(string(bs)), " ") {
		if f == "tailscale" {
			continue
		}
		args = append(args, f)
	}

	// List all resolvconf snippets except our own, and parse that as
	// a resolv.conf. This effectively generates a blended config of
	// "everyone except tailscale", which is what would be in use if
	// tailscale hadn't set exclusive mode.
	//
	// Note that this is not _entirely_ true. To be perfectly correct,
	// we should be looking for other interfaces marked exclusive that
	// predated tailscale, and stick to only those. However, in
	// practice, openresolv uses are generally quite limited, and boil
	// down to 1-2 DHCP leases, for which the correct outcome is a
	// blended config like the one we produce here.
	var buf bytes.Buffer
	cmd := exec.Command("resolvconf", args...)
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return OSConfig{}, err
	}
	return readResolv(&buf)
}

func (m openresolvManager) Close() error {
	cmd := exec.Command("resolvconf", "-f", "-d", "tailscale")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running %s: %s", cmd, out)
	}
	return nil
}
