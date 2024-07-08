package e2e

import (
	"context"
	"fmt"
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

	// Wait for the Service to be ready
	if err := wait.PollUntilContextCancel(ctx, time.Minute, true, func(ctx context.Context) (done bool, err error) {
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
	tsName := fmt.Sprintf("http://default-%s:80", svc.Name)
	resp, err := http.Get(tsName)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		respB, _ := httputil.DumpResponse(resp, true)
		t.Fatalf("unexpected status: %v; response body %s", resp.StatusCode, respB)
	}

}
