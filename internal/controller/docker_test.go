package controller

import (
	"strings"
	"testing"

	"forgejo-arc/internal/forgejo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
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

func TestDockerSocketOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := newFixture(t)
		f.pool.Spec.Mode = "Docker"
		f.pool.Spec.DockerSocket = enabled
		worker := workerJob(f.pool, forgejo.Job{Handle: "build"})
		pod := worker.Spec.Template.Spec
		secret, err := runnerSecret(f.pool, worker.Name, forgejo.Runner{})
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Container struct {
				DockerHost string `json:"docker_host"`
			} `json:"container"`
		}
		if err := yaml.Unmarshal(secret.Data["config.yaml"], &config); err != nil {
			t.Fatal(err)
		}
		if !enabled {
			if config.Container.DockerHost != "-" {
				t.Fatal("Docker socket must be hidden by default")
			}
			for _, mount := range pod.Containers[0].VolumeMounts {
				if mount.Name == "docker-socket" {
					t.Fatal("default runner must not mount Docker socket")
				}
			}
			continue
		}
		const socket = "unix:///run/forgejo-docker/docker.sock"
		if config.Container.DockerHost != socket {
			t.Fatalf("runner must mount the per-worker socket: %s", config.Container.DockerHost)
		}
		args := strings.Join(pod.InitContainers[0].Args, " ")
		if !strings.Contains(args, "--host="+socket) || !strings.Contains(args, "--group=1000") {
			t.Fatal("daemon must listen on a socket accessible to the runner's group")
		}
		for _, container := range []corev1.Container{pod.InitContainers[0], pod.Containers[0]} {
			found := false
			for _, mount := range container.VolumeMounts {
				found = found || (mount.Name == "docker-socket" && mount.MountPath == "/run/forgejo-docker")
			}
			if !found {
				t.Fatalf("%s must share the socket directory", container.Name)
			}
		}
		found := false
		for _, volume := range pod.Volumes {
			if volume.HostPath != nil {
				t.Fatal("must never expose a Kubernetes node socket")
			}
			found = found || (volume.Name == "docker-socket" && volume.EmptyDir != nil)
		}
		if !found {
			t.Fatal("socket must use a per-worker emptyDir")
		}
	}
}
