package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/forgejo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerpkg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const poolFinalizer = "arc.forgejo.org/runners"

type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Reader bypasses the informer cache for provisioning decisions.
	Reader   client.Reader
	Recorder record.EventRecorder
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&arc.RunnerPool{}).Owns(&batchv1.Job{}).
		WithOptions(controllerpkg.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

func (r *Reconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool arc.RunnerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			deleteMetrics(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	interval := time.Duration(pool.Spec.PollIntervalSeconds) * time.Second
	if interval == 0 {
		interval = 10 * time.Second
	}
	oldStatus := pool.DeepCopy().Status
	err := r.sync(ctx, &pool)
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Watching Forgejo jobs", ObservedGeneration: pool.Generation}
	if ptr.Deref(pool.Spec.MaxRunners, 1) == 0 {
		condition.Reason = "Paused"
		condition.Message = "New work is paused"
	}
	if !pool.DeletionTimestamp.IsZero() {
		condition.Reason = "Draining"
		condition.Message = "Draining workers before deletion"
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ReconcileFailed"
		condition.Message = err.Error()
	}
	previous := meta.FindStatusCondition(oldStatus.Conditions, "Ready")
	if err != nil && (previous == nil || previous.Message != condition.Message) {
		r.event(&pool, corev1.EventTypeWarning, "ReconcileFailed", condition.Message)
	}
	meta.SetStatusCondition(&pool.Status.Conditions, condition)
	degraded := metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse, Reason: "RetryBudgetAvailable", Message: "No exhausted infrastructure retries", ObservedGeneration: pool.Generation}
	if pool.Status.ExhaustedJobs > 0 {
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = "RetriesExhausted"
		degraded.Message = fmt.Sprintf("%d waiting jobs exhausted their infrastructure retry budget", pool.Status.ExhaustedJobs)
		if oldStatus.ExhaustedJobs != pool.Status.ExhaustedJobs {
			r.event(&pool, corev1.EventTypeWarning, "RetriesExhausted", degraded.Message)
		}
	}
	meta.SetStatusCondition(&pool.Status.Conditions, degraded)
	pool.Status.ObservedGeneration = pool.Generation
	// Write a complete status, including zero-valued required counts. A merge
	// patch from a zero Go struct would omit those fields on a brand-new pool.
	if !equality.Semantic.DeepEqual(oldStatus, pool.Status) {
		if statusErr := r.Status().Update(ctx, &pool); statusErr != nil && !apierrors.IsNotFound(statusErr) && err == nil {
			err = statusErr
		}
	}
	updateMetrics(&pool, err == nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *Reconciler) sync(ctx context.Context, pool *arc.RunnerPool) error {
	if !pool.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(pool, poolFinalizer) {
		return nil
	}
	var credentials corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.CredentialsSecret}, &credentials); err != nil {
		return fmt.Errorf("read credentials Secret: %w", err)
	}
	if len(credentials.Data["token"]) == 0 {
		return fmt.Errorf("credentials Secret must contain a nonempty token")
	}
	fj, err := forgejo.New(pool.Spec.ForgejoURL, string(credentials.Data["token"]), pool.Spec.Scope.Type, pool.Spec.Scope.Name)
	if err != nil {
		return err
	}
	var workers batchv1.JobList
	if err = r.reader().List(ctx, &workers, client.InNamespace(pool.Namespace), client.MatchingLabels{poolLabel: string(pool.UID)}); err != nil {
		return err
	}
	jobs, err := fj.Jobs(ctx, nil)
	if err != nil {
		return err
	}
	// Add the finalizer only once credentials and scope are usable. Invalid new
	// pools must remain deletable; previously provisioned pools retain protection.
	if !controllerutil.ContainsFinalizer(pool, poolFinalizer) && pool.DeletionTimestamp.IsZero() {
		if err := validateLabels(pool.Spec.Labels); err != nil {
			return err
		}
		controllerutil.AddFinalizer(pool, poolFinalizer)
		if err := r.Update(ctx, pool); err != nil {
			return err
		}
	}
	if err = r.sweep(ctx, pool, fj, workers.Items); err != nil {
		return err
	}
	byHandle := map[string]forgejo.Job{}
	pool.Status.WaitingJobs = 0
	pool.Status.ExhaustedJobs = 0
	for _, job := range jobs {
		byHandle[job.Handle] = job
		if job.Status == "waiting" && forgejo.Matches(job.RunsOn, pool.Spec.Labels) {
			pool.Status.WaitingJobs++
		}
	}
	pool.Status.ActiveRunners = 0
	for i := range workers.Items {
		worker := &workers.Items[i]
		remote, found := byHandle[worker.Annotations[handleAnnotation]]
		if !terminal(worker) {
			stop, stopErr := r.shouldStop(ctx, pool, worker, remote, found)
			if stopErr != nil {
				return stopErr
			}
			if stop || (!pool.DeletionTimestamp.IsZero() && ptr.Deref(worker.Spec.Suspend, false)) {
				done, stopErr := r.stop(ctx, worker)
				if stopErr != nil {
					return stopErr
				}
				if !done {
					pool.Status.ActiveRunners++
					continue
				}
			}
		}
		if terminal(worker) {
			if err = r.cleanup(ctx, fj, worker); err != nil {
				return err
			}
			// Retain the attempt record until it is no longer in Forgejo's active list.
			if !found || !pool.DeletionTimestamp.IsZero() || !worker.DeletionTimestamp.IsZero() {
				if err = r.deleteWorker(ctx, worker); err != nil {
					return err
				}
			}
			continue
		}
		pool.Status.ActiveRunners++
		if ptr.Deref(worker.Spec.Suspend, false) && found && remote.Status == "waiting" {
			if err = r.provision(ctx, pool, fj, worker); err != nil {
				return err
			}
		}
	}
	if !pool.DeletionTimestamp.IsZero() {
		if len(workers.Items) == 0 {
			controllerutil.RemoveFinalizer(pool, poolFinalizer)
			return r.Update(ctx, pool)
		}
		return nil
	}
	maximum := ptr.Deref(pool.Spec.MaxRunners, 1)
	if err := validateLabels(pool.Spec.Labels); err != nil {
		return err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	for _, job := range jobs {
		if job.Status != "waiting" || job.Handle == "" || !forgejo.Matches(job.RunsOn, pool.Spec.Labels) {
			continue
		}
		// One elected, serial reconciler plus an uncached read makes the Job a
		// durable claim shared by overlapping pools, including other namespaces.
		var claims batchv1.JobList
		if err = r.reader().List(ctx, &claims, client.MatchingLabels{attemptLabel: attemptKey(pool, job.Handle)}); err != nil {
			return err
		}
		attempt := nextAttempt(pool, claims.Items)
		if attempt == 0 {
			if exhausted(pool, claims.Items) {
				pool.Status.ExhaustedJobs++
			}
			continue
		}
		if pool.Status.ActiveRunners >= maximum {
			continue
		}
		worker := retryWorker(pool, job, attempt)
		if err = controllerutil.SetControllerReference(pool, worker, r.Scheme); err != nil {
			return err
		}
		if err = r.Create(ctx, worker); err != nil {
			return client.IgnoreAlreadyExists(err)
		}
		pool.Status.ActiveRunners++
		if err = r.provision(ctx, pool, fj, worker); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) provision(ctx context.Context, pool *arc.RunnerPool, fj *forgejo.Client, worker *batchv1.Job) error {
	var secret corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(worker), &secret)
	if apierrors.IsNotFound(err) {
		// A crash after registration but before Secret persistence loses the token.
		// Remove that registration by its unique worker identity before replacing it.
		runners, listErr := fj.Runners(ctx)
		if listErr != nil {
			return listErr
		}
		for _, runner := range runners {
			if runner.Name == worker.Name && runner.Description == "forgejo-arc:"+string(pool.UID) {
				if err = fj.Delete(ctx, runner.ID); err != nil {
					return err
				}
			}
		}
		runner, registerErr := fj.Register(ctx, worker.Name, "forgejo-arc:"+string(pool.UID))
		if registerErr != nil {
			return registerErr
		}
		snapshotPool := pool.DeepCopy()
		if snapshot := worker.Annotations[specAnnotation]; snapshot != "" {
			if err := json.Unmarshal([]byte(snapshot), &snapshotPool.Spec); err != nil {
				return fmt.Errorf("read worker spec snapshot: %w", err)
			}
		}
		created, buildErr := runnerSecret(snapshotPool, worker.Name, runner)
		if buildErr != nil {
			return buildErr
		}
		if err = controllerutil.SetControllerReference(worker, created, r.Scheme); err != nil {
			return err
		}
		if err = r.Create(ctx, created); err != nil {
			return err
		}
		secret = *created
	} else if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&secret, worker) || secret.Labels[poolLabel] != string(pool.UID) {
		return fmt.Errorf("worker Secret %s is not owned by this worker", secret.Name)
	}
	before := worker.DeepCopy()
	worker.Annotations[runnerAnnotation] = secret.Annotations[runnerAnnotation]
	worker.Spec.Suspend = ptr.To(false)
	if err := r.Patch(ctx, worker, client.MergeFrom(before)); err != nil {
		return err
	}
	startedCounter.WithLabelValues(pool.Namespace, pool.Name).Inc()
	r.event(pool, corev1.EventTypeNormal, "WorkerStarted", worker.Name)
	ctrl.LoggerFrom(ctx).Info("Started ephemeral worker", "worker", worker.Name, "namespace", worker.Namespace)
	return nil
}

func (r *Reconciler) cleanup(ctx context.Context, fj *forgejo.Client, worker *batchv1.Job) error {
	if worker.Annotations[cleanAnnotation] == "true" {
		return nil
	}
	id := worker.Annotations[runnerAnnotation]
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKeyFromObject(worker), &secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && (!metav1.IsControlledBy(&secret, worker) || secret.Labels[poolLabel] != worker.Labels[poolLabel]) {
		return fmt.Errorf("refusing to delete unowned Secret %s", secret.Name)
	}
	if id == "" {
		id = secret.Annotations[runnerAnnotation]
	}
	if id != "" {
		n, parseErr := strconv.ParseInt(id, 10, 64)
		if parseErr != nil {
			return parseErr
		}
		if err = fj.Delete(ctx, n); err != nil {
			return err
		}
	} else {
		// Registration may have succeeded immediately before a process crash.
		runners, listErr := fj.Runners(ctx)
		if listErr != nil {
			return listErr
		}
		for _, runner := range runners {
			if runner.Name == worker.Name && runner.Description == "forgejo-arc:"+worker.Labels[poolLabel] {
				if err = fj.Delete(ctx, runner.ID); err != nil {
					return err
				}
			}
		}
	}
	if secret.Name != "" {
		if err = r.Delete(ctx, &secret); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	worker.Annotations[cleanAnnotation] = "true"
	controllerutil.RemoveFinalizer(worker, workerFinalizer)
	return r.Update(ctx, worker)
}

func validateLabels(labels []string) error {
	for _, label := range labels {
		if label == "" || strings.ContainsAny(label, ",:") {
			return fmt.Errorf("labels must be nonempty plain names without commas or colons")
		}
	}
	return nil
}

func (r *Reconciler) event(pool *arc.RunnerPool, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(pool, kind, reason, message)
	}
}
