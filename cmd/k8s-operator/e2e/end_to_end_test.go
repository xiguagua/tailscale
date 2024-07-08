package e2e

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func TestIngress(t *testing.T) {
	cfg := config.GetConfigOrDie()
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Apply nginx
	// Apply service to expose it as ingress
	// Wait for tailnet node to become available
	// Ping it
}
