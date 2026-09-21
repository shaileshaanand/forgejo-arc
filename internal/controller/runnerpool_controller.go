package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/forgejo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const poolFinalizer = "arc.forgejo.org/runners"

type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&arc.RunnerPool{}).Owns(&batchv1.Job{}).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool arc.RunnerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	interval := time.Duration(pool.Spec.PollIntervalSeconds) * time.Second
	if interval == 0 {
		interval = 10 * time.Second
	}
	oldStatus := pool.DeepCopy().Status
	err := r.sync(ctx, &pool)
	before := pool.DeepCopy()
	before.Status = oldStatus
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Watching Forgejo jobs", ObservedGeneration: pool.Generation}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ReconcileFailed"
		condition.Message = err.Error()
	}
	meta.SetStatusCondition(&pool.Status.Conditions, condition)
	pool.Status.ObservedGeneration = pool.Generation
	if statusErr := r.Status().Patch(ctx, &pool, client.MergeFrom(before)); statusErr != nil && !apierrors.IsNotFound(statusErr) && err == nil {
		err = statusErr
	}
	return ctrl.Result{RequeueAfter: interval}, err
}

func (r *Reconciler) sync(ctx context.Context, pool *arc.RunnerPool) error {
	if err := validateLabels(pool.Spec.Labels); err != nil {
		return err
	}
	if !controllerutil.ContainsFinalizer(pool, poolFinalizer) && pool.DeletionTimestamp.IsZero() {
		controllerutil.AddFinalizer(pool, poolFinalizer)
		if err := r.Update(ctx, pool); err != nil {
			return err
		}
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
	if err = r.List(ctx, &workers, client.InNamespace(pool.Namespace), client.MatchingLabels{poolLabel: string(pool.UID)}); err != nil {
		return err
	}
	jobs, err := fj.Jobs(ctx, pool.Spec.Labels)
	if err != nil {
		return err
	}
	byHandle := map[string]forgejo.Job{}
	pool.Status.WaitingJobs = 0
	for _, job := range jobs {
		byHandle[job.Handle] = job
		if job.Status == "waiting" && forgejo.Matches(job.RunsOn, pool.Spec.Labels) {
			pool.Status.WaitingJobs++
		}
	}
	existing := map[string]bool{}
	pool.Status.ActiveRunners = 0
	for i := range workers.Items {
		worker := &workers.Items[i]
		existing[worker.Annotations[handleAnnotation]] = true
		if terminal(worker) {
			if err = r.cleanup(ctx, fj, worker); err != nil {
				return err
			}
			// Retain the attempt record until it is no longer in Forgejo's active list.
			if _, active := byHandle[worker.Annotations[handleAnnotation]]; !active {
				if err = r.Delete(ctx, worker, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
			continue
		}
		pool.Status.ActiveRunners++
		if ptr.Deref(worker.Spec.Suspend, false) {
			if !pool.DeletionTimestamp.IsZero() {
				if err = r.cleanup(ctx, fj, worker); err != nil {
					return err
				}
				if err = r.Delete(ctx, worker); client.IgnoreNotFound(err) != nil {
					return err
				}
				continue
			}
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
	if pool.Status.ActiveRunners >= 1 {
		return nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	for _, job := range jobs {
		if job.Status != "waiting" || job.Handle == "" || existing[job.Handle] || !forgejo.Matches(job.RunsOn, pool.Spec.Labels) {
			continue
		}
		worker := workerJob(pool, job)
		if err = controllerutil.SetControllerReference(pool, worker, r.Scheme); err != nil {
			return err
		}
		if err = r.Create(ctx, worker); err != nil {
			return client.IgnoreAlreadyExists(err)
		}
		pool.Status.ActiveRunners++
		return r.provision(ctx, pool, fj, worker)
	}
	return nil
}

func (r *Reconciler) provision(ctx context.Context, pool *arc.RunnerPool, fj *forgejo.Client, worker *batchv1.Job) error {
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKeyFromObject(worker), &secret)
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
		created, buildErr := runnerSecret(pool, worker.Name, runner)
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
	worker.Annotations[runnerAnnotation] = secret.Annotations[runnerAnnotation]
	worker.Spec.Suspend = ptr.To(false)
	return r.Update(ctx, worker)
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
	}
	if secret.Name != "" {
		if err = r.Delete(ctx, &secret); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	worker.Annotations[cleanAnnotation] = "true"
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
