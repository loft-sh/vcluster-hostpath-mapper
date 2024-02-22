package hostpaths

import (
	"context"
	"testing"

	"gotest.tools/assert"
	"gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_filter(t *testing.T) {
	testPodList := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod1",
				Namespace: "test-ns1",
			},
			Spec: corev1.PodSpec{},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod2",
				Namespace: "test-ns2",
			},
			Spec: corev1.PodSpec{},
		},
	}

	testCases := []struct {
		name               string
		podList            []corev1.Pod
		vclusterNamespaces map[string]struct{}
		expected           []corev1.Pod
	}{
		{
			name:    "None of the pods belong to namespace(s) managed by the current vCluster",
			podList: testPodList,
			vclusterNamespaces: map[string]struct{}{
				"test-ns3": {},
				"test-ns4": {},
			},
			expected: []corev1.Pod{},
		},
		{
			name:    "Some of the pods belong to namespace(s) managed by the current vCluster",
			podList: testPodList,
			vclusterNamespaces: map[string]struct{}{
				"test-ns1": {},
				"test-ns4": {},
			},
			expected: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod1",
						Namespace: "test-ns1",
					},
					Spec: corev1.PodSpec{},
				},
			},
		},
		{
			name:    "All of the pods belong to namespace(s) managed by the current vCluster",
			podList: testPodList,
			vclusterNamespaces: map[string]struct{}{
				"test-ns1": {},
				"test-ns2": {},
			},
			expected: testPodList,
		},
	}

	for _, testCase := range testCases {
		actual := filter(context.Background(), testCase.podList, testCase.vclusterNamespaces)

		assert.Assert(t,
			cmp.DeepEqual(actual, testCase.expected),
			"Unexpected result in test case %s",
			testCase.name,
		)
	}
}
