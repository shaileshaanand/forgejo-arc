package controller

import (
	arc "forgejo-arc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func addDocker(pool *arc.RunnerPool, pod *corev1.PodSpec) {
	image := pool.Spec.DockerImage
	if image == "" {
		image = "docker.io/library/docker:28.5.2-dind"
	}
	resources := *pool.Spec.DockerResources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}
	}
	pod.InitContainers = append(pod.InitContainers, corev1.Container{
		Name: "docker", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
		// A native sidecar is stopped automatically after the runner exits.
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
		Args:            []string{"dockerd", "--host=tcp://127.0.0.1:2375", "--host=unix:///var/run/docker.sock", "--tls=false", "--storage-driver=overlay2"},
		Env:             []corev1.EnvVar{{Name: "DOCKER_TLS_CERTDIR", Value: ""}},
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true), RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false)},
		Resources:       resources,
		StartupProbe:    &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"docker", "info"}}}, PeriodSeconds: 2, TimeoutSeconds: 2, FailureThreshold: 60},
		VolumeMounts:    []corev1.VolumeMount{{Name: "docker-data", MountPath: "/var/lib/docker"}},
	})
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "docker-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	pod.Containers[0].Env = append(pod.Containers[0].Env, corev1.EnvVar{Name: "DOCKER_HOST", Value: "tcp://127.0.0.1:2375"})
	if pool.Spec.DockerSocket {
		// The runner connects through this socket and mounts it from the DinD
		// filesystem into job containers at /var/run/docker.sock.
		mount := corev1.VolumeMount{Name: "docker-socket", MountPath: "/run/forgejo-docker"}
		sidecar := &pod.InitContainers[len(pod.InitContainers)-1]
		sidecar.Args = append(sidecar.Args, "--host=unix:///run/forgejo-docker/docker.sock", "--group=1000")
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, mount)
		pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts, mount)
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "docker-socket", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
}
