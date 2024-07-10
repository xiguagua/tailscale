// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build ios || android || js

package magicsock

import (
	"net/netip"

	"tailscale.com/types/logger"
)

type cloudInfo struct{}

func newCloudInfo(_ logger.Logf) *cloudInfo {
	return &cloudInfo{}
}

func (ci *cloudInfo) GetPublicIPs() ([]netip.Addr, error) {
	return nil, nil
}
