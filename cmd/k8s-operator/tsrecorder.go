package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	xslices "golang.org/x/exp/slices"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"tailscale.com/ipn"
	tsoperator "tailscale.com/k8s-operator"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime"
	"tailscale.com/types/ptr"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/mak"
	"tailscale.com/util/set"
)

const (
	reasonTSRecorderCreationFailed = "TSRecorderCreationFailed"
	reasonTSRecorderCreated        = "TSRecorderCreated"
	reasonTSRecorderInvalid        = "TSRecorderInvalid"
)

var gaugeTSRecorderResources = clientmetric.NewGauge("k8s_tsrecorder_resources")

// TSRecorderReconciler syncs TSRecorder statefulsets with their definition in
// TSRecorder CRs.
type TSRecorderReconciler struct {
	client.Client
	l           *zap.SugaredLogger
	recorder    record.EventRecorder
	clock       tstime.Clock
	tsNamespace string
	tsClient    tsClient

	mu          sync.Mutex           // protects following
	tsRecorders set.Slice[types.UID] // for tsrecorders gauge
}

func (r *TSRecorderReconciler) logger(name string) *zap.SugaredLogger {
	return r.l.With("TSRecorder", name)
}

func (r *TSRecorderReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, err error) {
	logger := r.logger(req.Name)
	logger.Debugf("starting reconcile")
	defer logger.Debugf("reconcile finished")

	tsr := new(tsapi.TSRecorder)
	err = r.Get(ctx, req.NamespacedName, tsr)
	if apierrors.IsNotFound(err) {
		logger.Debugf("TSRecorder not found, assuming it was deleted")
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get tailscale.com TSRecorder: %w", err)
	}
	if markedForDeletion(tsr) {
		logger.Debugf("TSRecorder is being deleted, cleaning up resources")
		ix := xslices.Index(tsr.Finalizers, FinalizerName)
		if ix < 0 {
			logger.Debugf("no finalizer, nothing to do")
			return reconcile.Result{}, nil
		}

		if done, err := r.maybeCleanup(ctx, tsr); err != nil {
			return reconcile.Result{}, err
		} else if !done {
			logger.Debugf("TSRecorder resource cleanup not yet finished, will retry...")
			return reconcile.Result{RequeueAfter: shortRequeue}, nil
		}

		tsr.Finalizers = slices.Delete(tsr.Finalizers, ix, ix+1)
		if err := r.Update(ctx, tsr); err != nil {
			return reconcile.Result{}, err
		}
		logger.Infof("TSRecorder resources cleaned up")
		return reconcile.Result{}, nil
	}

	oldTSRStatus := tsr.Status.DeepCopy()
	setStatus := func(tsr *tsapi.TSRecorder, _ tsapi.ConditionType, status metav1.ConditionStatus, reason, message string) (reconcile.Result, error) {
		tsoperator.SetTSRecorderCondition(tsr, tsapi.TSRecorderReady, status, reason, message, tsr.Generation, r.clock, logger)
		if !apiequality.Semantic.DeepEqual(oldTSRStatus, tsr.Status) {
			// An error encountered here should get returned by the Reconcile function.
			if updateErr := r.Client.Status().Update(ctx, tsr); updateErr != nil {
				err = errors.Wrap(err, updateErr.Error())
			}
		}
		return reconcile.Result{}, err
	}

	if !slices.Contains(tsr.Finalizers, FinalizerName) {
		// This log line is printed exactly once during initial provisioning,
		// because once the finalizer is in place this block gets skipped. So,
		// this is a nice place to tell the operator that the high level,
		// multi-reconcile operation is underway.
		logger.Infof("ensuring TSRecorder is set up")
		tsr.Finalizers = append(tsr.Finalizers, FinalizerName)
		if err := r.Update(ctx, tsr); err != nil {
			logger.Errorf("error adding finalizer: %w", err)
			return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionFalse, reasonTSRecorderCreationFailed, reasonTSRecorderCreationFailed)
		}
	}

	if err := r.validate(tsr); err != nil {
		logger.Errorf("error validating TSRecorder spec: %w", err)
		message := fmt.Sprintf("TSRecorder is invalid: %s", err)
		r.recorder.Eventf(tsr, corev1.EventTypeWarning, reasonTSRecorderInvalid, message)
		return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionFalse, reasonTSRecorderInvalid, message)
	}

	if err = r.maybeProvision(ctx, tsr); err != nil {
		logger.Errorf("error creating TSRecorder resources: %w", err)
		message := fmt.Sprintf("failed creating TSRecorder: %s", err)
		r.recorder.Eventf(tsr, corev1.EventTypeWarning, reasonTSRecorderCreationFailed, message)
		return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionFalse, reasonTSRecorderCreationFailed, message)
	}

	logger.Info("TSRecorder resources synced")
	return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionTrue, reasonTSRecorderCreated, reasonTSRecorderCreated)
}

func (r *TSRecorderReconciler) maybeProvision(ctx context.Context, tsr *tsapi.TSRecorder) error {
	// logger := r.logger(tsr.Name)
	// hostname := tsr.Name + "-tsrecorder"
	// if tsr.Spec.Hostname != "" {
	// 	hostname = string(tsr.Spec.Hostname)
	// }

	r.mu.Lock()
	r.tsRecorders.Add(tsr.UID)
	gaugeTSRecorderResources.Set(int64(r.tsRecorders.Len()))
	r.mu.Unlock()

	_, err := r.createOrGetSecrets(ctx, tsr)
	if err != nil {
		return fmt.Errorf("error creating secrets: %w", err)
	}
	sa := tsrServiceAccount(tsr)
	if _, err := createOrUpdate(ctx, r.Client, tsr.Namespace, &sa, func(s *corev1.ServiceAccount) {
		s.ObjectMeta.Labels = sa.ObjectMeta.Labels
		s.ObjectMeta.Annotations = sa.ObjectMeta.Annotations
	}); err != nil {
		return fmt.Errorf("error creating service account: %w", err)
	}
	ss := tsrStatefulSet(tsr)
	if _, err := createOrUpdate(ctx, r.Client, tsr.Namespace, &ss, func(s *appsv1.StatefulSet) {
		s.ObjectMeta.Labels = ss.ObjectMeta.Labels
		s.ObjectMeta.Annotations = ss.ObjectMeta.Annotations
		s.Spec = ss.Spec
	}); err != nil {
		return fmt.Errorf("error creating stateful set: %w", err)
	}

	// TODO
	// _, tsHost, ips, err := r.ssr.DeviceInfo(ctx, crl)
	// if err != nil {
	// 	return err
	// }

	// if tsHost == "" {
	// 	logger.Debugf("no Tailscale hostname known yet, waiting for connector pod to finish auth")
	// 	// No hostname yet. Wait for the connector pod to auth.
	// 	tsr.Status.TailnetIPs = nil
	// 	tsr.Status.Hostname = ""
	// 	return nil
	// }

	// tsr.Status.TailnetIPs = ips
	// tsr.Status.Hostname = tsHost

	return nil
}

func (r *TSRecorderReconciler) maybeCleanup(ctx context.Context, tsr *tsapi.TSRecorder) (bool, error) {
	logger := r.logger(tsr.Name)
	// TODO
	// if done, err := r.ssr.Cleanup(ctx, logger, childResourceLabels(tsr.Name, r.tsNamespace, "tsrecorder")); err != nil {
	// 	return false, fmt.Errorf("failed to cleanup TSRecorder resources: %w", err)
	// } else if !done {
	// 	logger.Debugf("TSRecorder cleanup not done yet, waiting for next reconcile")
	// 	return false, nil
	// }

	// Unlike most log entries in the reconcile loop, this will get printed
	// exactly once at the very end of cleanup, because the final step of
	// cleanup removes the tailscale finalizer, which will make all future
	// reconciles exit early.
	logger.Infof("cleaned up TSRecorder resources")
	r.mu.Lock()
	r.tsRecorders.Remove(tsr.UID)
	gaugeTSRecorderResources.Set(int64(r.tsRecorders.Len()))
	r.mu.Unlock()
	return true, nil
}

type secretMeta struct {
	name    string
	hash    string
	configs tailscaleConfigs
}

func (r *TSRecorderReconciler) createOrGetSecrets(ctx context.Context, tsr *tsapi.TSRecorder) (map[int]secretMeta, error) {
	logger := r.logger(tsr.Name)
	secrets := make(map[int]secretMeta)
	for i := range int(tsr.Spec.Replicas) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("%s-%d", tsr.Name, i),
				Namespace:       tsr.Namespace,
				Labels:          labels("tsrecorder", tsr.Name),
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(tsr, tsapi.SchemeGroupVersion.WithKind("TSRecorder"))},
			},
		}
		var orig *corev1.Secret // unmodified copy of secret
		if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil {
			logger.Debugf("secret %s/%s already exists", secret.GetNamespace(), secret.GetName())
			orig = secret.DeepCopy()
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}

		var authKey string
		if orig == nil {
			// Initially it contains only tailscaled config, but when the
			// proxy starts, it will also store there the state, certs and
			// ACME account key.
			var sts appsv1.StatefulSet
			stsKey := types.NamespacedName{
				Namespace: tsr.Namespace,
				Name:      tsr.Name,
			}
			if err := r.Get(ctx, stsKey, &sts); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			} else if err == nil {
				// StatefulSet exists, so we have already created the secret.
				// If the secret is missing, they should delete the StatefulSet.
				logger.Errorf("tsrecorder secret doesn't exist, but the corresponding StatefulSet %s/%s already does. Something is wrong, please delete the TSRecorder CR", sts.GetNamespace(), sts.GetName())
				return nil, nil
			}
			// Create API Key secret which is going to be used by the statefulset
			// to authenticate with Tailscale.
			logger.Debugf("creating authkey for new tsrecorder")
			var err error
			authKey, err = newAuthKey(ctx, r.tsClient, tsr.Spec.Tags.Stringify())
			if err != nil {
				return nil, err
			}
		}
		configs, err := tsrTailscaledConfig(tsr.Spec.Hostname, authKey, orig)
		if err != nil {
			return nil, fmt.Errorf("error creating tailscaled config: %w", err)
		}
		hash, err := tailscaledConfigHash(configs)
		if err != nil {
			return nil, fmt.Errorf("error calculating hash of tailscaled configs: %w", err)
		}

		latest := tailcfg.CapabilityVersion(-1)
		var latestConfig ipn.ConfigVAlpha
		for key, val := range configs {
			fn := tsoperator.TailscaledConfigFileNameForCap(key)
			b, err := json.Marshal(val)
			if err != nil {
				return nil, fmt.Errorf("error marshalling tailscaled config: %w", err)
			}
			mak.Set(&secret.StringData, fn, string(b))
			if key > latest {
				latest = key
				latestConfig = val
			}
		}

		if orig != nil {
			logger.Debugf("patching the existing proxy Secret with tailscaled config %s", sanitizeConfigBytes(latestConfig))
			if err := r.Patch(ctx, secret, client.MergeFrom(orig)); err != nil {
				return nil, err
			}
		} else {
			logger.Debugf("creating a new Secret for the proxy with tailscaled config %s", sanitizeConfigBytes(latestConfig))
			if err := r.Create(ctx, secret); err != nil {
				return nil, err
			}
		}

		secrets[i] = secretMeta{
			name:    secret.Name,
			hash:    hash,
			configs: configs,
		}
	}
	return secrets, nil
}

func (r *TSRecorderReconciler) validate(_ *tsapi.TSRecorder) error {
	return nil
}

func markedForDeletion(tsr *tsapi.TSRecorder) bool {
	return !tsr.DeletionTimestamp.IsZero()
}

func tsrTailscaledConfig(hostname tsapi.Hostname, newAuthkey string, oldSecret *corev1.Secret) (tailscaleConfigs, error) {
	conf := &ipn.ConfigVAlpha{
		Version:             "alpha0",
		AcceptDNS:           "false",
		AcceptRoutes:        "false", // AcceptRoutes defaults to true
		Locked:              "false",
		Hostname:            ptr.To(string(hostname)),
		NoStatefulFiltering: "false",
	}

	if newAuthkey != "" {
		conf.AuthKey = &newAuthkey
	} else if oldSecret != nil {
		var err error
		latest := tailcfg.CapabilityVersion(-1)
		latestStr := ""
		for k, data := range oldSecret.Data {
			// write to StringData, read from Data as StringData is write-only
			if len(data) == 0 {
				continue
			}
			v, err := tsoperator.CapVerFromFileName(k)
			if err != nil {
				continue
			}
			if v > latest {
				latestStr = k
				latest = v
			}
		}
		// Allow for configs that don't contain an auth key. Perhaps
		// users have some mechanisms to delete them. Auth key is
		// normally not needed after the initial login.
		if latestStr != "" {
			conf.AuthKey, err = readAuthKey(oldSecret, latestStr)
			if err != nil {
				return nil, err
			}
		}
	}
	capVerConfigs := make(map[tailcfg.CapabilityVersion]ipn.ConfigVAlpha)
	capVerConfigs[95] = *conf
	return capVerConfigs, nil
}

func tsrStatefulSet(tsr *tsapi.TSRecorder) appsv1.StatefulSet {
	return appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            tsr.Name,
			Namespace:       tsr.Namespace,
			Labels:          labels("tsrecorder", tsr.Name),
			Annotations:     nil, // TODO
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(tsr, tsapi.SchemeGroupVersion.WithKind("TSRecorder"))},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](tsr.Spec.Replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels("tsrecorder", tsr.Name),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        tsr.Name,
					Namespace:   tsr.Namespace,
					Labels:      labels("tsrecorder", tsr.Name),
					Annotations: nil, // TODO
				},
				Spec: corev1.PodSpec{
					// TODO: security context, image pull secrets, resources, others?
					ServiceAccountName: tsr.Name,
					Containers: []corev1.Container{
						{
							// TODO: extra env, resources, probes, others?
							Name: "tsrecorder",
							Image: func() string {
								repo, tag := tsr.Spec.Image.Repo, tsr.Spec.Image.Tag
								if repo == "" {
									repo = "tailscale/tsrecorder"
								}
								if tag == "" {
									tag = "stable"
								}
								return fmt.Sprintf("%s:%s", repo, tag)
							}(),
							Env: []corev1.EnvVar{
								{
									Name: "TS_KUBE_SECRET",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											// Secret is named after the pod.
											FieldPath: "metadata.name",
										},
									},
								},
							},
							Command: []string{"/tsrecorder"},
							Args: func() []string {
								args := []string{
									"--statedir=/data/state",
								}
								if tsr.Spec.Backends.Disk.Path != "" {
									args = append(args, "--dst="+tsr.Spec.Backends.Disk.Path)
								}
								if tsr.Spec.EnableUI {
									args = append(args, "--ui")
								}
								return args
							}(),
							VolumeMounts: append([]corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
									ReadOnly:  false,
								},
							}, tsr.Spec.ExtraVolumeMounts...),
						},
					},
					Volumes: append([]corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					}, tsr.Spec.ExtraVolumes...),
				},
			},
		},
	}
}

func tsrServiceAccount(tsr *tsapi.TSRecorder) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            tsr.Name,
			Namespace:       tsr.Namespace,
			Labels:          labels("tsrecorder", tsr.Name),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(tsr, tsapi.SchemeGroupVersion.WithKind("TSRecorder"))},
		},
	}
}

func labels(app, instance string) map[string]string {
	// ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": "tailscale-operator",
	}
}
