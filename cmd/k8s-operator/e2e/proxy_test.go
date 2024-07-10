package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/clientcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// TestProxy requires some setup not handled by this test:
// - Kubernetes cluster with tailscale operator installed
// - Current kubeconfig context set to connect to that cluster (directly, no operator proxy)
// - Operator installed with --set apiServerProxyConfig.mode="true"
// - ACLs that define tag:e2e-test-proxy tag
// - ACLs with the grant documented below for impersonation
// - OAuth client ID and secret in TS_API_CLIENT_ID and TS_API_CLIENT_SECRET env
// - OAuth client must have device write for tag:e2e-test-proxy tag
func TestProxy(t *testing.T) {
	start := time.Now()
	ctx := context.Background()
	cfg := config.GetConfigOrDie()
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Connect to tailnet with test-specific tag so we trigger some
	// preconfigured ACLs when connecting to the API server proxy:
	// "grants": [{
	// 	 "src": ["tag:e2e-test-proxy"],
	// 	 "dst": ["tag:k8s-operator"],
	// 	 "app": {
	//     "tailscale.com/cap/kubernetes": [{
	//       "impersonate": {
	//         "groups": ["ts:e2e-test-proxy"],
	//       }
	//     }]
	//   }
	// }]

	credentials := clientcredentials.Config{
		ClientID:     os.Getenv("TS_API_CLIENT_ID"),
		ClientSecret: os.Getenv("TS_API_CLIENT_SECRET"),
		TokenURL:     "https://login.tailscale.com/api/v2/oauth/token",
		Scopes:       []string{"device"}, // TODO: Add acl scope and ensure ACLs as part of test
	}
	tsClient := tailscale.NewClient("-", nil)
	tsClient.HTTPClient = credentials.Client(context.Background())

	caps := tailscale.KeyCapabilities{
		Devices: tailscale.KeyDeviceCapabilities{
			Create: tailscale.KeyDeviceCreateCapabilities{
				Reusable:      false,
				Preauthorized: true,
				Ephemeral:     true,
				Tags:          []string{"tag:e2e-test-proxy"},
			},
		},
	}

	tailscale.I_Acknowledge_This_API_Is_Unstable = true
	authKey, authKeyMeta, err := tsClient.CreateKey(ctx, caps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tsClient.DeleteKey(ctx, authKeyMeta.ID); err != nil {
			t.Errorf("error deleting auth key: %s", err)
		}
	})
	t.Logf("%.1fs created auth key", time.Since(start).Seconds())

	ts := &tsnet.Server{
		Hostname:  "test-proxy",
		Ephemeral: true,
		Dir:       t.TempDir(),
		AuthKey:   authKey,
	}
	_, err = ts.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ts.Close(); err != nil {
			t.Errorf("error shutting down tsnet.Server: %s", err)
		}
	})
	t.Logf("%.1fs tsnet.Server started", time.Since(start).Seconds())

	// Get operator host name from kube secret.
	operatorSecret := corev1.Secret{
		ObjectMeta: objectMeta("tailscale", "operator"),
	}
	if err := get(ctx, cl, &operatorSecret); err != nil {
		t.Fatal(err)
	}

	// Create role and role binding to allow a user we'll impersonate to do stuff.
	updateAndCleanup(t, ctx, cl, &rbacv1.Role{
		ObjectMeta: objectMeta("tailscale", "read-secrets"),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Verbs:     []string{"get"},
			Resources: []string{"secrets"},
		}},
	})
	updateAndCleanup(t, ctx, cl, &rbacv1.RoleBinding{
		ObjectMeta: objectMeta("tailscale", "read-secrets"),
		Subjects: []rbacv1.Subject{{
			Kind: "Group",
			Name: "ts:test-proxy",
		}},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role",
			Name: "read-secret",
		},
	})

	// Make proxy kube config that impersonates a user through operator proxy.
	proxyCfg := &rest.Config{
		Host:      fmt.Sprintf("%s:443", hostNameFromOperatorSecret(t, operatorSecret)),
		Transport: ts.HTTPClient().Transport,
	}
	proxyCl, err := client.New(proxyCfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Expect success.
	allowedSecret := corev1.Secret{
		ObjectMeta: objectMeta("tailscale", "operator"),
	}
	if err := get(ctx, proxyCl, &allowedSecret); err != nil {
		t.Fatal(err)
	}

	// Expect forbidden.
	forbiddenSecret := corev1.Secret{
		ObjectMeta: objectMeta("default", "operator"),
	}
	if err := get(ctx, proxyCl, &forbiddenSecret); err == nil {
		t.Fatal("expected error fetching secret from default namespace")
	}
}

func hostNameFromOperatorSecret(t *testing.T, s corev1.Secret) string {
	key, ok := strings.CutPrefix(string(s.Data["_current-profile"]), "profile-")
	if !ok {
		t.Fatal(string(s.Data["_current-profile"]))
	}
	profiles := map[string]any{}
	if err := json.Unmarshal(s.Data["_profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	profile, ok := profiles[key]
	if !ok {
		t.Fatal(profiles)
	}

	return ((profile.(map[string]any))["Name"]).(string)
}

func objectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
	}
}

func updateAndCleanup(t *testing.T, ctx context.Context, cl client.Client, obj client.Object) {
	t.Helper()
	if err := cl.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cl.Delete(ctx, obj); err != nil {
			t.Errorf("error cleaning up %s %s/%s: %s", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	})
}

func get(ctx context.Context, cl client.Client, obj client.Object) error {
	return cl.Get(ctx, client.ObjectKeyFromObject(obj), obj)
}
