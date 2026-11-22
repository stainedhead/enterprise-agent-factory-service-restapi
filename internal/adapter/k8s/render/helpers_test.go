package render_test

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

func nestedSlice(o map[string]any, f ...string) ([]any, bool, error) {
	return unstructured.NestedSlice(o, f...)
}
func nestedMap(o map[string]any, f ...string) (map[string]any, bool, error) {
	return unstructured.NestedMap(o, f...)
}
func nestedString(o map[string]any, f ...string) (string, bool, error) {
	return unstructured.NestedString(o, f...)
}
func nestedBool(o map[string]any, f ...string) (bool, bool, error) {
	return unstructured.NestedBool(o, f...)
}
