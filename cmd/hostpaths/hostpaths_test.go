package hostpaths

import (
	"context"
	"os"
	"path/filepath"
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

func Test_cleanupOldPodPath_kubelet(t *testing.T) {
	setup := func(t *testing.T) (context.Context, string, string, string) {
		t.Helper()
		root := t.TempDir()
		physicalPodDir := filepath.Join(root, "physical", "physical-uid")
		assert.NilError(t, os.MkdirAll(filepath.Join(physicalPodDir, "volumes"), 0o755))

		kubeletPodsPath := filepath.Join(root, "virtual", "kubelet", "pods")
		virtualPodDir := filepath.Join(kubeletPodsPath, "virtual-uid")
		assert.NilError(t, os.MkdirAll(virtualPodDir, 0o755))
		assert.NilError(t, os.Symlink(filepath.Join(physicalPodDir, "volumes"), filepath.Join(virtualPodDir, "volumes")))

		ctx := context.WithValue(context.Background(), optionsKey, &VirtualClusterOptions{VirtualKubeletPodPath: kubeletPodsPath})
		return ctx, kubeletPodsPath, virtualPodDir, physicalPodDir
	}

	exists := func(path string) bool {
		_, err := os.Lstat(path)
		return err == nil
	}

	t.Run("keeps the directory while the physical pod directory exists", func(t *testing.T) {
		ctx, kubeletPodsPath, virtualPodDir, _ := setup(t)

		assert.NilError(t, cleanupOldPodPath(ctx, kubeletPodsPath, map[string]bool{}))
		assert.Assert(t, exists(filepath.Join(virtualPodDir, "volumes")))
	})

	t.Run("removes the symlinks and the directory once the physical pod directory is gone", func(t *testing.T) {
		ctx, kubeletPodsPath, virtualPodDir, physicalPodDir := setup(t)
		assert.NilError(t, os.RemoveAll(physicalPodDir))

		assert.NilError(t, cleanupOldPodPath(ctx, kubeletPodsPath, map[string]bool{}))
		assert.Assert(t, !exists(virtualPodDir))
	})

	t.Run("keeps the directory of a pod that still exists", func(t *testing.T) {
		ctx, kubeletPodsPath, virtualPodDir, physicalPodDir := setup(t)
		assert.NilError(t, os.RemoveAll(physicalPodDir))

		assert.NilError(t, cleanupOldPodPath(ctx, kubeletPodsPath, map[string]bool{virtualPodDir: true}))
		assert.Assert(t, exists(filepath.Join(virtualPodDir, "volumes")))
	})
}
