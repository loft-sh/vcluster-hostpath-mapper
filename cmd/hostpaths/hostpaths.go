package hostpaths

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loft-sh/vcluster/pkg/controllers/resources/namespaces"
	podtranslate "github.com/loft-sh/vcluster/pkg/controllers/resources/pods/translate"
	"github.com/loft-sh/vcluster/pkg/util/clienthelper"

	"github.com/loft-sh/vcluster/config"
	"github.com/loft-sh/vcluster/config/legacyconfig"
	"github.com/loft-sh/vcluster/pkg/util/blockingcacheclient"
	"github.com/loft-sh/vcluster/pkg/util/pluginhookclient"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
}

type key int

const (
	LogsMountPath    = "/var/log"
	PodLogsMountPath = "/var/log/pods"

	NodeIndexName                    = "spec.nodeName"
	HostpathMapperSelfNodeNameEnvVar = "VCLUSTER_HOSTPATH_MAPPER_CURRENT_NODE_NAME"

	// naming format <pod_name>_<namespace>_<container_name>-<containerdID(hash, with <docker/cri>:// prefix removed)>.log
	ContainerSymlinkSourceTemplate = "%s_%s_%s-%s.log"

	MultiNamespaceMode = "multi-namespace-mode"
	SyncerContainer    = "syncer"

	optionsKey key = iota

	PodNameEnv               = "POD_NAME"
	configSecretNameTemplate = "vc-config-%s"
	configFilename           = "config.yaml"
)

// map of physical pod names to the corresponding virtual pod
type PhysicalPodMap map[string]*PodDetail

type PodDetail struct {
	Target      string
	SymLinkName *string
	PhysicalPod corev1.Pod
}

type VirtualClusterOptions struct {
	legacyconfig.LegacyVirtualClusterOptions
	VirtualLogsPath          string
	VirtualPodLogsPath       string
	VirtualContainerLogsPath string
	VirtualKubeletPodPath    string
}

func NewHostpathMapperCommand() *cobra.Command {
	options := &VirtualClusterOptions{}
	init := false

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Map host to virtual pod logs",
		Args:  cobra.NoArgs,
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return Start(cobraCmd.Context(), options, init)
		},
	}

	cmd.Flags().StringVar(&options.ClientCaCert, "client-ca-cert", "/data/server/tls/client-certificate", "The path to the client ca certificate")
	cmd.Flags().StringVar(&options.ServerCaCert, "server-ca-cert", "/data/server/tls/certificate-authority", "The path to the server ca certificate")
	cmd.Flags().StringVar(&options.ServerCaKey, "server-ca-key", "/data/server/tls/client-key", "The path to the server ca key")

	cmd.Flags().StringVar(&options.TargetNamespace, "target-namespace", "", "The namespace to run the virtual cluster in (defaults to current namespace)")

	cmd.Flags().StringVar(&options.Name, "name", "vcluster", "The name of the virtual cluster")
	cmd.Flags().BoolVar(&init, "init", false, "If this is the init container")

	return cmd
}

func podNodeIndexer(obj client.Object) []string {
	res := []string{}
	pod := obj.(*corev1.Pod)
	if pod.Spec.NodeName != "" {
		res = append(res, pod.Spec.NodeName)
	}

	return res
}

func Start(ctx context.Context, options *VirtualClusterOptions, init bool) error {
	// get current namespace
	currentNamespace, err := clienthelper.CurrentNamespace()
	if err != nil {
		return err
	}

	// ensure target namespace
	if options.TargetNamespace == "" {
		options.TargetNamespace = currentNamespace
	}

	virtualPath := fmt.Sprintf(podtranslate.VirtualPathTemplate, options.TargetNamespace, options.Name)
	options.VirtualKubeletPodPath = filepath.Join(virtualPath, "kubelet", "pods")
	options.VirtualLogsPath = filepath.Join(virtualPath, "log")
	options.VirtualPodLogsPath = filepath.Join(options.VirtualLogsPath, "pods")
	options.VirtualContainerLogsPath = filepath.Join(options.VirtualLogsPath, "containers")

	inClusterConfig := ctrl.GetConfigOrDie()

	inClusterConfig.QPS = 40
	inClusterConfig.Burst = 80
	inClusterConfig.Timeout = 0

	translate.VClusterName = options.Name

	var virtualClusterConfig *rest.Config
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Hour, true, func(context.Context) (bool, error) {
		virtualClusterConfig = &rest.Config{
			Host: options.Name,
			TLSClientConfig: rest.TLSClientConfig{
				ServerName: options.Name,
				CertFile:   options.ClientCaCert,
				KeyFile:    options.ServerCaKey,
				CAFile:     options.ServerCaCert,
			},
		}

		kubeClient, err := kubernetes.NewForConfig(virtualClusterConfig)
		if err != nil {
			return false, fmt.Errorf("create kube client: %w", err)
		}

		_, err = kubeClient.Discovery().ServerVersion()
		if err != nil {
			klog.Infof("couldn't retrieve virtual cluster version (%v), will retry in 1 seconds", err)
			return false, nil
		}
		_, err = kubeClient.CoreV1().ServiceAccounts("default").Get(ctx, "default", metav1.GetOptions{})
		if err != nil {
			klog.Infof("default ServiceAccount is not available yet, will retry in 1 seconds")
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(inClusterConfig)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}

	err = findVclusterModeAndSetDefaultTranslation(ctx, kubeClient, options)
	if err != nil {
		return fmt.Errorf("find vcluster mode: %w", err)
	}

	localManager, err := ctrl.NewManager(inClusterConfig, localManagerCtrlOptions(options))
	if err != nil {
		return err
	}

	virtualClusterManager, err := ctrl.NewManager(virtualClusterConfig, ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		NewClient:      pluginhookclient.NewVirtualPluginClientFactory(blockingcacheclient.NewCacheClient),
	})
	if err != nil {
		return err
	}

	ctx = context.WithValue(ctx, optionsKey, options)

	startManagers(ctx, localManager, virtualClusterManager)

	if init {
		klog.Info("is init container mode")
		defer ctx.Done()
		return restartTargetPods(ctx, options, localManager, virtualClusterManager)
	}

	klog.Info("mapping hostpaths")
	err = os.Mkdir(options.VirtualContainerLogsPath, os.ModeDir)
	if err != nil {
		if !os.IsExist(err) {
			klog.Errorf("error creating container dir in log path: %v", err)
			return err
		}
	}

	return mapHostPaths(ctx, localManager, virtualClusterManager)
}

func getSyncerPodSpec(ctx context.Context, kubeClient kubernetes.Interface, vclusterName, vclusterNamespace string) (*corev1.PodSpec, error) {
	// try looking for the stateful set first

	vclusterSts, err := kubeClient.AppsV1().StatefulSets(vclusterNamespace).Get(ctx, vclusterName, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		// try looking for deployment - in case of eks/k8s
		vclusterDeploy, err := kubeClient.AppsV1().Deployments(vclusterNamespace).Get(ctx, vclusterName, metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			klog.Errorf("could not find vcluster either in statefulset or deployment: %v", err)
			return nil, err
		} else if err != nil {
			klog.Errorf("error looking for vcluster deployment: %v", err)
			return nil, err
		}

		return &vclusterDeploy.Spec.Template.Spec, nil
	} else if err != nil {
		return nil, err
	}

	return &vclusterSts.Spec.Template.Spec, nil
}

func getVclusterConfigFromSecret(ctx context.Context, kubeClient kubernetes.Interface, vclusterName, vclusterNamespace string) (*config.Config, error) {
	configSecret, err := kubeClient.CoreV1().Secrets(vclusterNamespace).Get(ctx, fmt.Sprintf(configSecretNameTemplate, vclusterName), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	rawBytes, ok := configSecret.Data[configFilename]
	if !ok {
		return nil, fmt.Errorf("key '%s' not found in secret", configFilename)
	}

	// create a new strict decoder
	rawConfig := &config.Config{}
	err = yaml.UnmarshalStrict(rawBytes, rawConfig)
	if err != nil {
		klog.Errorf("unmarshal %s: %#+v", configFilename, errors.Unwrap(err))
		return nil, err
	}

	return rawConfig, nil
}

func setMultiNamespaceMode(options *VirtualClusterOptions) {
	options.MultiNamespaceMode = true
	translate.Default = translate.NewMultiNamespaceTranslator(options.TargetNamespace)
}

func localManagerCtrlOptions(options *VirtualClusterOptions) manager.Options {
	controllerOptions := ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		NewClient:      pluginhookclient.NewPhysicalPluginClientFactory(blockingcacheclient.NewCacheClient),
	}

	if !options.MultiNamespaceMode {
		controllerOptions.Cache.DefaultNamespaces = map[string]cache.Config{options.TargetNamespace: {}}
	}

	return controllerOptions
}

func findVclusterModeAndSetDefaultTranslation(ctx context.Context, kubeClient kubernetes.Interface, options *VirtualClusterOptions) error {
	vClusterConfig, err := getVclusterConfigFromSecret(ctx, kubeClient, options.Name, options.TargetNamespace)
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	} else if vClusterConfig != nil && vClusterConfig.Experimental.MultiNamespaceMode.Enabled {
		setMultiNamespaceMode(options)
		return nil
	}

	vclusterPodSpec, err := getSyncerPodSpec(ctx, kubeClient, options.Name, options.TargetNamespace)
	if err != nil {
		return err
	}

	for _, container := range vclusterPodSpec.Containers {
		if container.Name == SyncerContainer {
			// iterate over command args
			for _, arg := range container.Args {
				if strings.Contains(arg, MultiNamespaceMode) {
					setMultiNamespaceMode(options)
					return nil
				}
			}
		}
	}

	translate.Default = translate.NewSingleNamespaceTranslator(options.TargetNamespace)
	return nil
}

func restartTargetPods(ctx context.Context, options *VirtualClusterOptions, localManager, virtualClusterManager manager.Manager) error {
	pPodList := &corev1.PodList{}

	err := localManager.GetClient().List(ctx, pPodList, &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{
			NodeIndexName: os.Getenv(HostpathMapperSelfNodeNameEnvVar),
		}),
	})

	if err != nil {
		klog.Errorf("unable to list pods: %v", err)
		return err
	}

	podRestartList := []corev1.Pod{}

podLoop:
	for _, pPod := range pPodList.Items {
		// skip current pod itself
		if pPod.Name == os.Getenv(PodNameEnv) {
			klog.Infof("skipping self pod %s", pPod.Name)
			continue
		}

		klog.Infof("processing pod %s", pPod.Name)

		for _, volume := range pPod.Spec.Volumes {
			if volume.VolumeSource.HostPath != nil {
				if volume.VolumeSource.HostPath.Path == podtranslate.PodLoggingHostPath ||
					volume.VolumeSource.HostPath.Path == podtranslate.LogHostPath ||
					volume.VolumeSource.HostPath.Path == podtranslate.KubeletPodPath {
					klog.Infof("adding pod %s to restart list", pPod.Name)
					podRestartList = append(podRestartList, pPod)
					continue podLoop
				}
			}
		}
	}

	klog.Infof("restart list %d", len(podRestartList))

	// translate to physical pod name and delete
	// this would require us to know wether multinamespace mode or single namespace mode?
	for _, pPod := range podRestartList {
		klog.Infof("deleting physical pod %s", pPod.Name)

		err = localManager.GetClient().Delete(ctx, &pPod)
		if err != nil {
			klog.Errorf("error deleting target pod %s: %v", pPod.Name, err)
		}
	}

	return nil
}

func mapHostPaths(ctx context.Context, pManager, vManager manager.Manager) error {
	options := ctx.Value(optionsKey).(*VirtualClusterOptions)

	mapFunc := func() error {
		podMappings, err := getPhysicalPodMap(ctx, options, pManager)
		if err != nil {
			klog.Errorf("unable to get physical pod mapping: %v", err)
			return nil
		}

		vPodList := &corev1.PodList{}
		err = vManager.GetClient().List(ctx, vPodList, &client.ListOptions{
			FieldSelector: fields.SelectorFromSet(fields.Set{
				NodeIndexName: os.Getenv(HostpathMapperSelfNodeNameEnvVar),
			}),
		})
		if err != nil {
			klog.Errorf("unable to list pods: %v", err)
			return nil
		}

		existingVPodsWithNamespace := make(map[string]bool)
		existingPodsPath := make(map[string]bool)
		existingKubeletPodsPath := make(map[string]bool)

		for _, vPod := range vPodList.Items {
			existingVPodsWithNamespace[fmt.Sprintf("%s_%s", vPod.Name, vPod.Namespace)] = true
			pName := translate.Default.HostName(nil, vPod.Name, vPod.Namespace).Name

			if podDetail, ok := podMappings[pName]; ok {
				// create pod log symlink
				source := filepath.Join(options.VirtualPodLogsPath, fmt.Sprintf("%s_%s_%s", vPod.Namespace, vPod.Name, string(vPod.UID)))
				target := filepath.Join(podtranslate.PhysicalPodLogVolumeMountPath, podDetail.Target)

				existingPodsPath[source] = true

				_, err := createPodLogSymlinkToPhysical(source, target)
				if err != nil {
					return fmt.Errorf("unable to create symlink for %s: %w", podDetail.Target, err)
				}

				// create kubelet pod symlink
				kubeletPodSymlinkSource := filepath.Join(options.VirtualKubeletPodPath, string(vPod.GetUID()))
				kubeletPodSymlinkTarget := filepath.Join(podtranslate.PhysicalKubeletVolumeMountPath, string(podDetail.PhysicalPod.GetUID()))
				existingKubeletPodsPath[kubeletPodSymlinkSource] = true
				err = createKubeletVirtualToPhysicalPodLinks(kubeletPodSymlinkSource, kubeletPodSymlinkTarget)
				if err != nil {
					return err
				}

				// podDetail.SymLinkName = symlinkName

				// create container to vPod symlinks
				containerSymlinkTargetDir := filepath.Join(PodLogsMountPath,
					fmt.Sprintf("%s_%s_%s", vPod.Namespace, vPod.Name, string(vPod.UID)))
				err = createContainerToPodSymlink(ctx, vPod, podDetail, containerSymlinkTargetDir)
				if err != nil {
					return err
				}
			}
		}

		// cleanup old pod symlinks
		err = cleanupOldPodPath(ctx, options.VirtualPodLogsPath, existingPodsPath)
		if err != nil {
			klog.Errorf("error cleaning up old pod log paths: %v", err)
		}

		err = cleanupOldContainerPaths(ctx, existingVPodsWithNamespace)
		if err != nil {
			klog.Errorf("error cleaning up old container log paths: %v", err)
		}

		err = cleanupOldPodPath(ctx, options.VirtualKubeletPodPath, existingKubeletPodsPath)
		if err != nil {
			klog.Errorf("error cleaning up old kubelet pod paths: %v", err)
		}

		klog.Infof("successfully reconciled mapper")
		return nil
	}

	for {
		err := mapFunc()
		if err != nil {
			return err
		}

		time.Sleep(5 * time.Second)
	}
}

func getPhysicalPodMap(ctx context.Context, options *VirtualClusterOptions, pManager manager.Manager) (PhysicalPodMap, error) {
	podListOptions := &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{
			NodeIndexName: os.Getenv(HostpathMapperSelfNodeNameEnvVar),
		}),
	}

	if !options.MultiNamespaceMode {
		podListOptions.Namespace = options.TargetNamespace
	}

	podList := &corev1.PodList{}
	err := pManager.GetClient().List(ctx, podList, podListOptions)
	if err != nil {
		return nil, fmt.Errorf("unable to list pods: %w", err)
	}

	var pods []corev1.Pod
	if options.MultiNamespaceMode {
		// find namespaces managed by the current vcluster
		nsList := &corev1.NamespaceList{}
		err = pManager.GetClient().List(ctx, nsList, &client.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{
				namespaces.VClusterNamespaceAnnotation: options.TargetNamespace,
			}),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to list namespaces: %w", err)
		}

		vclusterNamespaces := make(map[string]struct{}, len(nsList.Items))
		for _, ns := range nsList.Items {
			vclusterNamespaces[ns.Name] = struct{}{}
		}

		// Limit Pods
		pods = filter(ctx, podList.Items, vclusterNamespaces)
	} else {
		pods = podList.Items
	}

	podMappings := make(PhysicalPodMap, len(pods))
	for _, pPod := range pods {
		lookupName := fmt.Sprintf("%s_%s_%s", pPod.Namespace, pPod.Name, pPod.UID)

		ok, err := checkIfPathExists(lookupName)
		if err != nil {
			klog.Errorf("error checking existence for path %s: %v", lookupName, err)
		}

		if ok {
			// check entry in podMapping
			if _, ok := podMappings[pPod.Name]; !ok {
				podMappings[pPod.Name] = &PodDetail{
					Target:      lookupName,
					PhysicalPod: pPod,
				}
			}
		}
	}

	return podMappings, nil
}

func filter(ctx context.Context, podList []corev1.Pod, vclusterNamespaces map[string]struct{}) []corev1.Pod {
	pods := make([]corev1.Pod, 0, len(podList))
	for _, pod := range podList {
		if _, ok := vclusterNamespaces[pod.Namespace]; ok {
			pods = append(pods, pod)
		}
	}

	return pods
}

func cleanupOldContainerPaths(ctx context.Context, existingVPodsWithNS map[string]bool) error {
	options := ctx.Value(optionsKey).(*VirtualClusterOptions)

	vPodsContainersOnDisk, err := os.ReadDir(options.VirtualContainerLogsPath)
	if err != nil {
		return err
	}

	for _, vPodContainerOnDisk := range vPodsContainersOnDisk {
		nameParts := strings.Split(vPodContainerOnDisk.Name(), "_")
		vPodOnDiskName, vPodOnDiskNS := nameParts[0], nameParts[1]

		if _, ok := existingVPodsWithNS[fmt.Sprintf("%s_%s", vPodOnDiskName, vPodOnDiskNS)]; !ok {
			// this pod no longer exists, hence this container
			// belonging to it should no longer exist either
			fullPathToCleanup := filepath.Join(options.VirtualContainerLogsPath, vPodContainerOnDisk.Name())

			klog.Infof("cleaning up %s", fullPathToCleanup)
			err := os.RemoveAll(fullPathToCleanup)
			if err != nil {
				klog.Errorf("error deleting symlink %s: %v", fullPathToCleanup, err)
			}
		}
	}

	return nil
}

func createKubeletVirtualToPhysicalPodLinks(vPodDirName, pPodDirName string) error {
	err := os.MkdirAll(vPodDirName, os.ModeDir)
	if err != nil {
		return fmt.Errorf("error creating vPod kubelet directory for %s: %w", vPodDirName, err)
	}

	// scan all contents in the physical pod dir
	// and create equivalent symlinks from virtual
	// path to physical
	contents, err := os.ReadDir(pPodDirName)
	if err != nil {
		return fmt.Errorf("error reading physical kubelet pod dir %s: %w", pPodDirName, err)
	}

	for _, content := range contents {
		fullKubeletVirtualPodPath := filepath.Join(vPodDirName, content.Name())
		fullKubeletPhysicalPodPath := filepath.Join(pPodDirName, content.Name())

		err := os.Symlink(
			fullKubeletPhysicalPodPath,
			fullKubeletVirtualPodPath)
		if err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("error creating symlink for %s -> %s: %w", fullKubeletVirtualPodPath, fullKubeletPhysicalPodPath, err)
			}
		} else {
			klog.Infof("created kubelet pod symlink %s -> %s", fullKubeletVirtualPodPath, fullKubeletPhysicalPodPath)
		}
	}

	return nil
}

func cleanupOldPodPath(ctx context.Context, cleanupDirPath string, existingPodPathsFromAPIServer map[string]bool) error {
	vPodDirsOnDisk, err := os.ReadDir(cleanupDirPath)
	if err != nil {
		return err
	}

	options := ctx.Value(optionsKey).(*VirtualClusterOptions)

	for _, vPodDirOnDisk := range vPodDirsOnDisk {
		fullVPodDirDiskPath := filepath.Join(cleanupDirPath, vPodDirOnDisk.Name())
		if _, ok := existingPodPathsFromAPIServer[fullVPodDirDiskPath]; !ok {
			if cleanupDirPath == options.VirtualKubeletPodPath {
				// check if the symlinks resolve
				// this extra check for kubelet is because velero backups
				// depend on it and we don't want to delete the virtual paths
				// which the physical paths are still not cleaned up by the
				// kubelet
				symlinks, err := os.ReadDir(fullVPodDirDiskPath)
				if err != nil {
					klog.Errorf("error iterating over vpod dir %s: %v", fullVPodDirDiskPath, err)
				}

				for _, sl := range symlinks {
					target := filepath.Join(fullVPodDirDiskPath, sl.Name())
					_, readLinkErr := os.Readlink(target)
					if readLinkErr != nil {
						// symlink no longer resolves, hence delete
						klog.Infof("cleaning up %s", target)
						err := os.RemoveAll(target)
						if err != nil {
							klog.Errorf("error deleting symlink %s: %v", target, err)
						}
					}
				}
				continue
			}

			// this symlink source exists on the disk but the vPod
			// lo longer exists as per the API server, hence delete
			// the symlink
			klog.Infof("cleaning up %s", fullVPodDirDiskPath)
			err := os.RemoveAll(fullVPodDirDiskPath)
			if err != nil {
				klog.Errorf("error deleting symlink %s: %v", fullVPodDirDiskPath, err)
			}
		}
	}

	return nil
}

func createContainerToPodSymlink(ctx context.Context, vPod corev1.Pod, pPodDetail *PodDetail, targetDir string) error {
	options := ctx.Value(optionsKey).(*VirtualClusterOptions)

	for _, containerStatus := range vPod.Status.ContainerStatuses {
		_, containerID, _ := strings.Cut(containerStatus.ContainerID, "://")
		containerName := containerStatus.Name

		source := fmt.Sprintf(ContainerSymlinkSourceTemplate,
			vPod.Name,
			vPod.Namespace,
			containerName,
			containerID)

		pPod := pPodDetail.PhysicalPod
		physicalContainerFileName := fmt.Sprintf(ContainerSymlinkSourceTemplate,
			pPod.Name,
			pPod.Namespace,
			containerName,
			containerID)

		physicalLogFileName, err := getPhysicalLogFilename(ctx, physicalContainerFileName)
		if err != nil {
			klog.Errorf("error reading destination filename from physical container symlink: %v", err)
			continue
		}

		target := filepath.Join(targetDir, containerName, physicalLogFileName)
		source = filepath.Join(options.VirtualContainerLogsPath, source)

		err = os.Symlink(target, source)
		if err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("error creating container:%s to pod:%s symlink: %w", source, target, err)
			}

			continue
		}

		klog.Infof("created container:%s -> pod:%s symlink", source, target)
	}

	return nil
}

// we need to get the info that which log file in the physical pod dir
// should this virtual container symlink point to. for eg.
// <physical_container> -> /var/log/pods/<pod>/<container>/xxx.log
// <virtual_container> -> <virtual_pod_path>/<container>/xxx.log
func getPhysicalLogFilename(ctx context.Context, physicalContainerFileName string) (string, error) {
	pContainerFilePath := filepath.Join(LogsMountPath, "containers", physicalContainerFileName)
	pDestination, err := os.Readlink(pContainerFilePath)
	if err != nil {
		return "", err
	}

	splits := strings.Split(pDestination, "/")
	fileName := splits[len(splits)-1]

	return fileName, nil
}

// check if folder exists
func checkIfPathExists(path string) (bool, error) {
	fullPath := filepath.Join(PodLogsMountPath, path)

	if _, err := os.Stat(fullPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func startManagers(ctx context.Context, pManager, vManager manager.Manager) {
	err := pManager.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, NodeIndexName, podNodeIndexer)
	if err != nil {
		panic(err)
	}

	go func() {
		err := pManager.Start(ctx)
		if err != nil {
			panic(err)
		}
	}()

	err = vManager.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, NodeIndexName, podNodeIndexer)
	if err != nil {
		panic(err)
	}

	go func() {
		err := vManager.Start(ctx)
		if err != nil {
			panic(err)
		}
	}()
}

func createPodLogSymlinkToPhysical(vPodDirName, pPodDirName string) (*string, error) {
	err := os.Symlink(pPodDirName, vPodDirName)
	if err != nil {
		if os.IsExist(err) {
			return &vPodDirName, nil
		}

		return nil, err
	}

	klog.Infof("created symlink from %s -> %s", vPodDirName, pPodDirName)
	return &vPodDirName, nil
}
