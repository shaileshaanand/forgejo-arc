package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/forgejo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fixture struct {
	t                        *testing.T
	r                        *Reconciler
	pool                     *arc.RunnerPool
	jobs                     []forgejo.Job
	registrations, deletions int
	runners                  []forgejo.Runner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, jobs: []forgejo.Job{{ID: 1, Handle: "first-attempt", Status: "waiting", RunsOn: []string{"linux"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == "POST":
			f.registrations++
			var r forgejo.Runner
			json.NewDecoder(req.Body).Decode(&r)
			r.ID = int64(f.registrations)
			r.UUID = "uuid"
			r.Token = "worker-token"
			f.runners = append(f.runners, r)
			json.NewEncoder(w).Encode(r)
		case req.Method == "DELETE":
			f.deletions++
			w.WriteHeader(204)
		case strings.HasSuffix(req.URL.Path, "/jobs"):
			json.NewEncoder(w).Encode(f.jobs)
		case req.URL.Query().Get("page") == "1":
			json.NewEncoder(w).Encode(f.runners)
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	arc.AddToScheme(scheme)
	batchv1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	f.pool = &arc.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "test", UID: "pool-uid"}, Spec: arc.RunnerPoolSpec{ForgejoURL: server.URL, Scope: arc.Scope{Type: "Repository", Name: "owner/repo"}, CredentialsSecret: "credentials", Labels: []string{"linux"}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "test"}, Data: map[string][]byte{"token": []byte("controller-token")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&arc.RunnerPool{}, &batchv1.Job{}).WithObjects(f.pool, secret).Build()
	f.r = &Reconciler{Client: c, Scheme: scheme}
	return f
}

func (f *fixture) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "test"}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) workers() []batchv1.Job {
	f.t.Helper()
	var jobs batchv1.JobList
	if err := f.r.List(context.Background(), &jobs); err != nil {
		f.t.Fatal(err)
	}
	return jobs.Items
}

func TestAttemptLifecycle(t *testing.T) {
	f := newFixture(t)
	f.reconcile()
	f.reconcile()
	workers := f.workers()
	if len(workers) != 1 || f.registrations != 1 {
		t.Fatalf("duplicate provisioning: workers=%d registrations=%d", len(workers), f.registrations)
	}
	worker := workers[0]
	if *worker.Spec.Suspend || *worker.Spec.BackoffLimit != 0 {
		t.Fatal("worker not started with retry disabled")
	}
	if *worker.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("worker has Kubernetes credentials")
	}
	var secret corev1.Secret
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&worker), &secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(secret.Data["config.yaml"]), "controller-token") {
		t.Fatal("controller token leaked to worker")
	}
	if !strings.Contains(strings.Join(worker.Spec.Template.Spec.Containers[0].Args, " "), "--handle first-attempt") {
		t.Fatal("worker not bound to attempt")
	}
	worker.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := f.r.Status().Update(context.Background(), &worker); err != nil {
		t.Fatal(err)
	}
	f.jobs = nil
	f.reconcile()
	if f.deletions != 1 || len(f.workers()) != 0 {
		t.Fatal("completed worker not cleaned up")
	}
	var secrets corev1.SecretList
	f.r.List(context.Background(), &secrets, client.MatchingLabels{poolLabel: "pool-uid"})
	if len(secrets.Items) != 0 {
		t.Fatal("runner secret leaked")
	}
}

func TestFailedWorkerDoesNotReplayActiveAttempt(t *testing.T) {
	f := newFixture(t)
	f.reconcile()
	worker := f.workers()[0]
	worker.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := f.r.Status().Update(context.Background(), &worker); err != nil {
		t.Fatal(err)
	}
	f.jobs[0].Status = "running"
	f.reconcile()
	f.reconcile()
	if f.registrations != 1 {
		t.Fatal("replayed a running attempt")
	}
}

func TestConcurrencyAndOverlappingPools(t *testing.T) {
	f := newFixture(t)
	f.pool.Spec.MaxRunners = ptr.To(int32(2))
	if err := f.r.Update(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	f.jobs = append(f.jobs, forgejo.Job{ID: 2, Handle: "second", Status: "waiting", RunsOn: []string{"linux"}}, forgejo.Job{ID: 3, Handle: "third", Status: "waiting", RunsOn: []string{"linux"}})
	f.reconcile()
	f.reconcile()
	if len(f.workers()) != 2 {
		t.Fatal("concurrency was not bounded at two")
	}
	other := f.pool.DeepCopy()
	other.Name = "overlap"
	other.Namespace = "other"
	other.UID = "other-pool"
	other.ResourceVersion = ""
	other.Spec.MaxRunners = ptr.To(int32(5))
	if err := f.r.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "other"}, Data: map[string][]byte{"token": []byte("controller-token")}}
	if err := f.r.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.workers()) != 3 || f.registrations != 3 {
		t.Fatal("overlapping pools duplicated an attempt")
	}
}

func TestZeroPausesAndRerunsUseNewIdentity(t *testing.T) {
	f := newFixture(t)
	f.pool.Spec.MaxRunners = ptr.To(int32(0))
	if err := f.r.Update(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if len(f.workers()) != 0 {
		t.Fatal("paused pool started work")
	}
	if workerName(f.pool, "attempt-1") == workerName(f.pool, "attempt-2") {
		t.Fatal("reruns must have independent workers")
	}
}
