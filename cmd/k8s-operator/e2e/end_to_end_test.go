package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	kube "tailscale.com/k8s-operator"
	"tailscale.com/tstest"
)

func TestIngress(t *testing.T) {
	cfg := config.GetConfigOrDie()
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Apply nginx
	// Apply service to expose it as ingress
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			Annotations: map[string]string{
				"tailscale.com/expose": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name": "ingress-nginx",
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Protocol: "TCP",
					Port:     80,
				},
			},
		},
	}

	ctx := context.Background()

	err = cl.Create(ctx, svc, &client.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating service: %v", err)
	}
	t.Cleanup(func() {
		if err := cl.Delete(ctx, svc); err != nil {
			t.Errorf("failed cleaning up Service: %v", err)
		}
	})

	// Poll every 2 seconds till test times out.
	// TODO: instead of timing out only when test times out, cancel context after 60s or so.
	if err := wait.PollUntilContextCancel(ctx, time.Second*2, true, func(ctx context.Context) (done bool, err error) {
		maybeReadySvc := &corev1.Service{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(svc), maybeReadySvc); err != nil {
			return false, err
		}
		isReady := kube.SvcIsReady(maybeReadySvc)
		if isReady {
			t.Log("Service is ready")
		}
		return isReady, nil
	}); err != nil {
		t.Fatalf("error waiting for the Service to become Ready: %v", err)
	}

	// TODO: instead of doing this, create an HTTP client with a custom resolver
	DNS := "100.100.100.100"
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Millisecond * time.Duration(3000),
			}
			return d.DialContext(ctx, "udp", fmt.Sprintf("%s:53", DNS))
		},
	}

	tsName := fmt.Sprintf("http://default-%s:80/healthz", svc.Name)

	var resp *http.Response
	if err := tstest.WaitFor(time.Second*60, func() error {
		resp, err = http.Get(tsName)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("error trying to reach service: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %v; response body s", resp.StatusCode)
	}
	respB, _ := httputil.DumpResponse(resp, true)
	t.Logf(string(respB))
}
