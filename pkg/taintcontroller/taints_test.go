package taintcontroller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEnsureTaint(t *testing.T) {
	node := &corev1.Node{}
	changed := EnsureTaint(node, "key", "val", corev1.TaintEffectNoSchedule)
	if !changed {
		t.Fatalf("expected taint to be added")
	}
	if !HasTaint(node, "key", "val", corev1.TaintEffectNoSchedule) {
		t.Fatalf("expected taint to be present after EnsureTaint")
	}

	changed = EnsureTaint(node, "key", "val", corev1.TaintEffectNoSchedule)
	if changed {
		t.Fatalf("did not expect second EnsureTaint call to change node")
	}
}

func TestRemoveTaint(t *testing.T) {
	node := &corev1.Node{
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "keep", Value: "yes", Effect: corev1.TaintEffectNoSchedule},
				{Key: "drop", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}

	changed := RemoveTaint(node, "drop", "true", corev1.TaintEffectNoSchedule)
	if !changed {
		t.Fatalf("expected taint removal to report change")
	}
	if HasTaint(node, "drop", "true", corev1.TaintEffectNoSchedule) {
		t.Fatalf("expected target taint to be removed")
	}
	if !HasTaint(node, "keep", "yes", corev1.TaintEffectNoSchedule) {
		t.Fatalf("expected non-target taints to remain")
	}

	changed = RemoveTaint(node, "missing", "x", corev1.TaintEffectNoSchedule)
	if changed {
		t.Fatalf("expected no change when taint absent")
	}
}
