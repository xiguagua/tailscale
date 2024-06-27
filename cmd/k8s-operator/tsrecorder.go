package main

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	xslices "golang.org/x/exp/slices"
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
	ssr         *tailscaleSTSReconciler
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

		if done, err := r.maybeCleanupTSRecorder(ctx, logger, tsr); err != nil {
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

	if err = r.maybeProvisionTSRecorder(ctx, logger, tsr); err != nil {
		logger.Errorf("error creating TSRecorder resources: %w", err)
		message := fmt.Sprintf("failed creating TSRecorder: %s", err)
		r.recorder.Eventf(tsr, corev1.EventTypeWarning, reasonTSRecorderCreationFailed, message)
		return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionFalse, reasonTSRecorderCreationFailed, message)
	}

	logger.Info("TSRecorder resources synced")
	return setStatus(tsr, tsapi.TSRecorderReady, metav1.ConditionTrue, reasonTSRecorderCreated, reasonTSRecorderCreated)
}

func (r *TSRecorderReconciler) maybeProvisionTSRecorder(ctx context.Context, logger *zap.SugaredLogger, tsr *tsapi.TSRecorder) error {
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

	_, err := r.ssr.Provision(ctx, logger, sts)
	if err != nil {
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

func (r *TSRecorderReconciler) maybeCleanupTSRecorder(ctx context.Context, logger *zap.SugaredLogger, tsr *tsapi.TSRecorder) (bool, error) {
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
