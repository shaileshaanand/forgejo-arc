package controller

import (
	"context"
	"time"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/forgejo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stop waits for pods to terminate before revoking their registration. Suspending
// first prevents the Job controller from creating a replacement pod.
func (r *Reconciler) stop(ctx context.Context, worker *batchv1.Job) (bool, error) {
	if !ptr.Deref(worker.Spec.Suspend, false) || worker.Annotations[stoppingAnnotation] != "true" {
		worker.Spec.Suspend = ptr.To(true)
		worker.Annotations[stoppingAnnotation] = "true"
		if err := r.Update(ctx, worker); err != nil {
			return false, err
		}
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(worker.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": string(worker.UID)}); err != nil {
		return false, err
	}
	active := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, worker) {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		active = true
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
	}
	if active {
		return false, nil
	}
	worker.Annotations[stoppedAnnotation] = "true"
	return true, r.Update(ctx, worker)
}

// shouldStop uses only a successful, unfiltered Forgejo snapshot. A missing
// attempt has a grace period so finishing runners can flush their final logs.
func (r *Reconciler) shouldStop(ctx context.Context, pool *arc.RunnerPool, worker *batchv1.Job, remote forgejo.Job, found bool) (bool, error) {
	if !worker.DeletionTimestamp.IsZero() || worker.Annotations[stoppingAnnotation] == "true" {
		return true, nil
	}
	if !found {
		value := worker.Annotations[missingAnnotation]
		if value == "" {
			worker.Annotations[missingAnnotation] = time.Now().UTC().Format(time.RFC3339)
			return false, r.Update(ctx, worker)
		}
		since, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return true, nil
		}
		return time.Since(since) > 30*time.Second, nil
	}
	if worker.Annotations[missingAnnotation] != "" {
		delete(worker.Annotations, missingAnnotation)
		if err := r.Update(ctx, worker); err != nil {
			return false, err
		}
	}
	startup := pool.Spec.StartupTimeoutSeconds
	if startup == 0 {
		startup = 180
	}
	return remote.Status == "waiting" && !worker.CreationTimestamp.IsZero() && time.Since(worker.CreationTimestamp.Time) > time.Duration(startup)*time.Second, nil
}

func nextAttempt(pool *arc.RunnerPool, claims []batchv1.Job) int {
	maximum := pool.Spec.MaxAttempts
	if maximum == 0 {
		maximum = 3
	}
	if len(claims) >= int(maximum) {
		return 0
	}
	for _, worker := range claims {
		if worker.Labels[poolLabel] != string(pool.UID) || !terminal(&worker) || worker.Annotations[cleanAnnotation] != "true" {
			return 0
		}
		// A successful runner process may have reported a failed workflow. Either
		// way it must not be retried against a stale waiting snapshot.
		for _, condition := range worker.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				return 0
			}
		}
		if !worker.CreationTimestamp.IsZero() && time.Since(worker.CreationTimestamp.Time) < 30*time.Second {
			return 0
		}
	}
	return len(claims) + 1
}

func exhausted(pool *arc.RunnerPool, claims []batchv1.Job) bool {
	maximum := pool.Spec.MaxAttempts
	if maximum == 0 {
		maximum = 3
	}
	if len(claims) < int(maximum) {
		return false
	}
	for _, worker := range claims {
		if worker.Labels[poolLabel] != string(pool.UID) || !terminal(&worker) {
			return false
		}
		for _, condition := range worker.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				return false
			}
		}
	}
	return true
}

func (r *Reconciler) sweep(ctx context.Context, pool *arc.RunnerPool, fj *forgejo.Client, workers []batchv1.Job) error {
	runners, err := fj.Runners(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(workers))
	for _, worker := range workers {
		known[worker.Name] = true
	}
	for _, runner := range runners {
		if runner.Description == "forgejo-arc:"+string(pool.UID) && !known[runner.Name] {
			if err := fj.Delete(ctx, runner.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) deleteWorker(ctx context.Context, worker *batchv1.Job) error {
	return client.IgnoreNotFound(r.Delete(ctx, worker, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}
