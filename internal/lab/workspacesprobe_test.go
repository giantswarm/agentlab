package lab

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// restrictedViolations lists what keeps the pod out of the PodSecurity
// `restricted` profile as Giant Swarm installations enforce it through
// Kyverno: require-run-as-nonroot, disallow-privilege-escalation,
// disallow-capabilities-strict, restrict-seccomp-strict and
// restrict-volume-types.
func restrictedViolations(pod *corev1.Pod) []string {
	var v []string
	psc := pod.Spec.SecurityContext
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil && vol.EmptyDir == nil && vol.ConfigMap == nil && vol.Secret == nil && vol.Projected == nil {
			v = append(v, "volume "+vol.Name+" is of a type restricted refuses")
		}
	}
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		sc := c.SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		nonRoot := sc.RunAsNonRoot
		if nonRoot == nil {
			nonRoot = psc.RunAsNonRoot
		}
		if nonRoot == nil || !*nonRoot {
			v = append(v, c.Name+": runAsNonRoot is not true")
		}
		user := sc.RunAsUser
		if user == nil {
			user = psc.RunAsUser
		}
		if user == nil || *user == 0 {
			v = append(v, c.Name+": no non-root runAsUser")
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			v = append(v, c.Name+": allowPrivilegeEscalation is not false")
		}
		if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, "ALL") {
			v = append(v, c.Name+": capabilities do not drop ALL")
		} else if len(sc.Capabilities.Add) > 0 {
			v = append(v, c.Name+": capabilities are added")
		}
		seccomp := sc.SeccompProfile
		if seccomp == nil {
			seccomp = psc.SeccompProfile
		}
		if seccomp == nil || (seccomp.Type != corev1.SeccompProfileTypeRuntimeDefault && seccomp.Type != corev1.SeccompProfileTypeLocalhost) {
			v = append(v, c.Name+": seccompProfile is not RuntimeDefault")
		}
		if sc.Privileged != nil && *sc.Privileged {
			v = append(v, c.Name+": privileged")
		}
	}
	return v
}

func TestWorkspaceVolumeProbesAreRestricted(t *testing.T) {
	gitMounts := []corev1.VolumeMount{
		{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, SubPath: workspacesTestSessions + "/s"},
		{Name: workspacesTestVolume, MountPath: workspacesActorMirrorsPath, SubPath: workspacesTestMirrors, ReadOnly: true},
		{Name: measureLocalVolume, MountPath: "/" + measureLocalVolume},
	}
	for name, pod := range map[string]*corev1.Pod{
		"measure dirs":  measureVolumePod("claim", "dirs", sessionsDirectoriesScript, nil),
		"measure clear": measureVolumePod("claim", "clear", accessClearSessionsScript, nil),
		"measure git":   measureVolumePod("claim", "git", measureGitScript("o/r", "main"), gitMounts),
		"sync clear":    workspaceVolumePod("agentlab-sync-clear", "claim", syncRemoveSessionScript),
	} {
		if v := restrictedViolations(pod); len(v) > 0 {
			t.Errorf("%s: not PodSecurity restricted: %v", name, v)
		}
		psc := pod.Spec.SecurityContext
		for field, got := range map[string]*int64{"runAsUser": psc.RunAsUser, "runAsGroup": psc.RunAsGroup, "fsGroup": psc.FSGroup} {
			if got == nil || *got != workspaceVolumeUser {
				t.Errorf("%s: %s is %v, want the sync Job's %d", name, field, got, workspaceVolumeUser)
			}
		}
		if pod.Spec.Volumes[0].PersistentVolumeClaim == nil || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "claim" {
			t.Errorf("%s: the first volume is not the workspace's claim: %+v", name, pod.Spec.Volumes[0])
		}
	}
	git := measureVolumePod("claim", "git", "true", gitMounts)
	if len(git.Spec.Volumes) != 2 || git.Spec.Volumes[1].EmptyDir == nil || len(git.Spec.Containers[0].VolumeMounts) != len(gitMounts) {
		t.Errorf("the git probe has volumes %+v and mounts %+v, want the claim, the local emptyDir and its mounts", git.Spec.Volumes, git.Spec.Containers[0].VolumeMounts)
	}
}

// The check itself refuses the probe pod as it was before the fix: no
// security context at all.
func TestRestrictedViolationsRefuseABarePod(t *testing.T) {
	pod := probePod("bare", workspacesTestGitImage, []string{"true"})
	if v := restrictedViolations(pod); len(v) != 5 {
		t.Errorf("a bare probe pod has violations %v, want runAsNonRoot, runAsUser, privilege escalation, capabilities and seccomp", v)
	}
}
