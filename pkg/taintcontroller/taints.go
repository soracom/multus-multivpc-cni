package taintcontroller

import (
	corev1 "k8s.io/api/core/v1"
)

// HasTaint returns true when the node already has the taint matching all fields.
func HasTaint(node *corev1.Node, key, value string, effect corev1.TaintEffect) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == key && taint.Value == value && taint.Effect == effect {
			return true
		}
	}
	return false
}

// EnsureTaint appends the taint to the node if missing and reports whether a mutation occurred.
func EnsureTaint(node *corev1.Node, key, value string, effect corev1.TaintEffect) bool {
	if HasTaint(node, key, value, effect) {
		return false
	}
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    key,
		Value:  value,
		Effect: effect,
	})
	return true
}

// RemoveTaint removes the taint when present and reports whether a mutation occurred.
func RemoveTaint(node *corev1.Node, key, value string, effect corev1.TaintEffect) bool {
	out := make([]corev1.Taint, 0, len(node.Spec.Taints))
	changed := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == key && taint.Value == value && taint.Effect == effect {
			changed = true
			continue
		}
		out = append(out, taint)
	}
	if changed {
		node.Spec.Taints = out
	}
	return changed
}
