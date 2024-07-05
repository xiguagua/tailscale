package main

import (
	"context"
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
	tsoperator "tailscale.com/k8s-operator"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/tstime"
	"tailscale.com/types/ptr"
	"tailscale.com/util/clientmetric"
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
	logger      *zap.SugaredLogger
	recorder    record.EventRecorder
	clock       tstime.Clock
	tsNamespace string

	mu          sync.Mutex           // protects following
	tsRecorders set.Slice[types.UID] // for tsrecorders gauge
}

func (r *TSRecorderReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, err error) {
	logger := r.logger.With("TSRecorder", req.Name)
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

		if done, err := r.maybeCleanup(ctx, logger, tsr); err != nil {
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

	if err = r.maybeProvision(ctx, logger, tsr); err != nil {
		logger.Errorf("error creating TSRecorder resources: %w", err)
		message := fmt.Sprintf("failed creating TSRecorder: %s", err)
		r.recorder.Eventf(tsr, corev1.EventTypeWarning, reasonTSRecorderCreationFailed, message)
		return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionFalse, reasonTSRecorderCreationFailed, message)
	}

	logger.Info("TSRecorder resources synced")
	return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionTrue, reasonTSRecorderCreated, reasonTSRecorderCreated)
}

func (r *TSRecorderReconciler) maybeProvision(ctx context.Context, logger *zap.SugaredLogger, tsr *tsapi.TSRecorder) error {
	hostname := tsr.Name + "-tsrecorder"
	if tsr.Spec.Hostname != "" {
		hostname = string(tsr.Spec.Hostname)
	}
	crl := childResourceLabels(tsr.Name, r.tsNamespace, "tsrecorder")

	sts := &tailscaleSTSConfig{
		ParentResourceName:  tsr.Name,
		ParentResourceUID:   string(tsr.UID),
		Hostname:            hostname,
		ChildResourceLabels: crl,
		Tags:                tsr.Spec.Tags.Stringify(),
	}

	r.mu.Lock()
	r.tsRecorders.Add(tsr.UID)
	gaugeTSRecorderResources.Set(int64(r.tsRecorders.Len()))
	r.mu.Unlock()

	depl := statefulSet(tsr)
	if _, err := createOrUpdate(ctx, r.Client, tsr.Namespace, &depl, func(d *appsv1.Deployment) {
		d.ObjectMeta.Labels = depl.ObjectMeta.Labels
		d.ObjectMeta.Annotations = depl.ObjectMeta.Annotations
		d.Spec = depl.Spec
	}); err != nil {
		return err
	}

	_, tsHost, ips, err := r.ssr.DeviceInfo(ctx, crl)
	if err != nil {
		return err
	}

	if tsHost == "" {
		logger.Debugf("no Tailscale hostname known yet, waiting for connector pod to finish auth")
		// No hostname yet. Wait for the connector pod to auth.
		tsr.Status.TailnetIPs = nil
		tsr.Status.Hostname = ""
		return nil
	}

	tsr.Status.TailnetIPs = ips
	tsr.Status.Hostname = tsHost

	return nil
}

func (r *TSRecorderReconciler) maybeCleanup(ctx context.Context, logger *zap.SugaredLogger, tsr *tsapi.TSRecorder) (bool, error) {
	if done, err := r.ssr.Cleanup(ctx, logger, childResourceLabels(tsr.Name, r.tsNamespace, "tsrecorder")); err != nil {
		return false, fmt.Errorf("failed to cleanup TSRecorder resources: %w", err)
	} else if !done {
		logger.Debugf("TSRecorder cleanup not done yet, waiting for next reconcile")
		return false, nil
	}

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

func (r *TSRecorderReconciler) validate(_ *tsapi.TSRecorder) error {
	return nil
}

func markedForDeletion(tsr *tsapi.TSRecorder) bool {
	return !tsr.DeletionTimestamp.IsZero()
}

func statefulSet(tsr *tsapi.TSRecorder) appsv1.StatefulSet {
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
							Env: []corev1.EnvVar{ // TODO: deploy and use a secret
								{
									Name: "TS_AUTHKEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "recorder",
											},
											Key: "key",
										},
									},
								},
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

func sa(tsr *tsapi.TSRecorder) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            tsr.Name,
			Namespace:       tsr.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(tsr, tsapi.SchemeGroupVersion.WithKind("TSRecorder"))},
			Labels:          labels("tsrecorder", tsr.Name),
		},
	}
}

func labels(app, instance string) map[string]string {
	// ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": "tailscale.com/operator",
	}
}
