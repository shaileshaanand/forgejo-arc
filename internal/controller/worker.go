package controller

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/forgejo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	poolLabel          = "arc.forgejo.org/pool-uid"
	attemptLabel       = "arc.forgejo.org/attempt"
	handleAnnotation   = "arc.forgejo.org/handle"
	runnerAnnotation   = "arc.forgejo.org/runner-id"
	cleanAnnotation    = "arc.forgejo.org/cleaned"
	stoppedAnnotation  = "arc.forgejo.org/stopped"
	stoppingAnnotation = "arc.forgejo.org/stopping"
	missingAnnotation  = "arc.forgejo.org/missing-since"
	workerFinalizer    = "arc.forgejo.org/registration"
	specAnnotation     = "arc.forgejo.org/spec-snapshot"
)

func workerName(pool *arc.RunnerPool, handle string) string {
	sum := sha256.Sum256([]byte(string(pool.UID) + "/" + handle))
	return fmt.Sprintf("arc-%x", sum[:16])
}

func attemptKey(pool *arc.RunnerPool, handle string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(pool.Spec.ForgejoURL, "/") + "/" + handle))
	return fmt.Sprintf("%x", sum[:24])
}

func workerJob(pool *arc.RunnerPool, job forgejo.Job) *batchv1.Job {
	snapshot, _ := json.Marshal(pool.Spec)
	name := workerName(pool, job.Handle)
	labels := map[string]string{poolLabel: string(pool.UID), attemptLabel: attemptKey(pool, job.Handle)}
	image := pool.Spec.RunnerImage
	if image == "" {
		image = "data.forgejo.org/forgejo/runner:13.0.0"
	}
	deadline := pool.Spec.JobTimeoutSeconds
	if deadline == 0 {
		deadline = 3600
	}
	resources := *pool.Spec.Resources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	}
	worker := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.Namespace, Labels: labels, Finalizers: []string{workerFinalizer}, Annotations: map[string]string{handleAnnotation: job.Handle, specAnnotation: string(snapshot)}},
		Spec: batchv1.JobSpec{
			Suspend: ptr.To(true), BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(deadline),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
				TerminationGracePeriodSeconds: ptr.To(int64(30)),
				SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1000)), RunAsGroup: ptr.To(int64(1000)), FSGroup: ptr.To(int64(1000)), RunAsNonRoot: ptr.To(true)},
				Containers: []corev1.Container{{Name: "runner", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            []string{"forgejo-runner", "one-job", "--config", "/etc/runner/config.yaml", "--handle", job.Handle, "--wait"},
					Resources:       resources,
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					VolumeMounts:    []corev1.VolumeMount{{Name: "config", MountPath: "/etc/runner", ReadOnly: true}, {Name: "work", MountPath: "/data"}},
				}},
				Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr.To(int32(0440))}}}, {Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			}},
		},
	}
	settings := pool.Spec.DeepCopy()
	worker.Spec.Template.Spec.NodeSelector = settings.NodeSelector
	worker.Spec.Template.Spec.Tolerations = settings.Tolerations
	worker.Spec.Template.Spec.Affinity = settings.Affinity
	worker.Spec.Template.Spec.ImagePullSecrets = settings.ImagePullSecrets
	if pool.Spec.Mode == "Docker" {
		addDocker(pool, &worker.Spec.Template.Spec)
	}
	return worker
}

func runnerSecret(pool *arc.RunnerPool, name string, runner forgejo.Runner) (*corev1.Secret, error) {
	labels := make([]string, len(pool.Spec.Labels))
	for i, label := range pool.Spec.Labels {
		labels[i] = label + ":host"
		if pool.Spec.Mode == "Docker" {
			image := pool.Spec.JobImage
			if image == "" {
				image = "docker.io/library/node:22-bookworm-slim"
			}
			labels[i] = label + ":docker://" + image
		}
	}
	timeout := pool.Spec.JobTimeoutSeconds
	if timeout == 0 {
		timeout = 3600
	}
	config := map[string]any{
		"runner": map[string]any{"capacity": 1, "timeout": fmt.Sprintf("%ds", timeout), "shutdown_timeout": "20s"},
		"cache":  map[string]any{"enabled": false},
		"host":   map[string]any{"workdir_parent": "/data/work"},
		"server": map[string]any{"connections": map[string]any{"forgejo": map[string]any{"url": pool.Spec.ForgejoURL, "uuid": runner.UUID, "token": runner.Token, "labels": labels}}},
	}
	if pool.Spec.Mode == "Docker" {
		config["container"] = map[string]any{"docker_host": "-"}
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.Namespace, Labels: map[string]string{poolLabel: string(pool.UID)}, Annotations: map[string]string{runnerAnnotation: strconv.FormatInt(runner.ID, 10)}}, Data: map[string][]byte{"config.yaml": data}}, nil
}

func terminal(job *batchv1.Job) bool {
	if job.Annotations[stoppedAnnotation] == "true" {
		return true
	}
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func retryWorker(pool *arc.RunnerPool, job forgejo.Job, attempt int) *batchv1.Job {
	worker := workerJob(pool, job)
	if attempt > 1 {
		worker.Name += fmt.Sprintf("-r%d", attempt)
		worker.Spec.Template.Spec.Volumes[0].Secret.SecretName = worker.Name
	}
	return worker
}
