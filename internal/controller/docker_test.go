package controller

import (
	"testing"

	"forgejo-arc/internal/forgejo"
	corev1 "k8s.io/api/core/v1"
)

func TestDockerSidecarIsolationAndTermination(t *testing.T) {
	f := newFixture(t)
	f.pool.Spec.Mode = "Docker"
	worker := workerJob(f.pool, forgejo.Job{Handle: "docker-attempt"})
	pod := worker.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || *pod.InitContainers[0].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatal("Docker must be a native sidecar so the Job can finish")
	}
	if !*pod.InitContainers[0].SecurityContext.Privileged || *pod.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege must be restricted to Docker sidecar")
	}
	if pod.InitContainers[0].StartupProbe == nil {
		t.Fatal("runner must wait for Docker readiness")
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			t.Fatal("worker must not mount host paths")
		}
	}
	for _, mount := range pod.InitContainers[0].VolumeMounts {
		if mount.Name == "config" {
			t.Fatal("sidecar must not mount runner credentials")
		}
	}
}
