// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"context"
	"testing"
)

func TestCloudInfo(t *testing.T) {
	// TODO(andrew-d): better test
	ci := newCloudInfo(t.Logf)
	ips, err := ci.GetPublicIPs(context.Background())
	t.Logf("ips: %v", ips)
	t.Logf("err: %v", err)
}
