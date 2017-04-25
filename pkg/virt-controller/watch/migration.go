package watch

import (
	"fmt"
	kubeapi "k8s.io/client-go/pkg/api"
	k8sv1 "k8s.io/client-go/pkg/api/v1"
	metav1 "k8s.io/client-go/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/labels"
	"k8s.io/client-go/pkg/types"
	"k8s.io/client-go/pkg/util/workqueue"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"k8s.io/client-go/kubernetes"
	"kubevirt.io/kubevirt/pkg/api/v1"
	corev1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/logging"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

func migrationVMPodSelector() kubeapi.ListOptions {
	fieldSelectionQuery := fmt.Sprintf("status.phase=%s", string(kubeapi.PodRunning))
	fieldSelector := fields.ParseSelectorOrDie(fieldSelectionQuery)
	labelSelectorQuery := fmt.Sprintf("%s, %s in (virt-launcher)", string(corev1.MigrationLabel), corev1.AppLabel)
	labelSelector, err := labels.Parse(labelSelectorQuery)

	if err != nil {
		panic(err)
	}
	return kubeapi.ListOptions{FieldSelector: fieldSelector, LabelSelector: labelSelector}
}

func NewMigrationController(migrationService services.VMService, recorder record.EventRecorder, restClient *rest.RESTClient, clientset *kubernetes.Clientset) (cache.Store, *kubecli.Controller, *workqueue.RateLimitingInterface) {
	lw := cache.NewListWatchFromClient(restClient, "migrations", k8sv1.NamespaceDefault, fields.Everything())
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	store, controller := kubecli.NewController(lw, queue, &v1.Migration{}, NewMigrationControllerDispatch(migrationService, restClient, clientset))
	return store, controller, &queue
}

func NewMigrationPodController(vmCache cache.Store, recorder record.EventRecorder, clientset *kubernetes.Clientset, restClient *rest.RESTClient, vmService services.VMService, migrationQueue workqueue.RateLimitingInterface) (cache.Store, *kubecli.Controller) {

	selector := migrationVMPodSelector()
	lw := kubecli.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", kubeapi.NamespaceDefault, selector.FieldSelector, selector.LabelSelector)
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	return kubecli.NewController(lw, queue, &k8sv1.Pod{}, NewTargetPodControllerDispatch(vmCache, restClient, vmService, clientset, migrationQueue))
}

func NewMigrationControllerDispatch(vmService services.VMService, restClient *rest.RESTClient, clientset *kubernetes.Clientset) kubecli.ControllerDispatch {

	dispatch := MigrationDispatch{
		restClient: restClient,
		vmService:  vmService,
		clientset:  clientset,
	}
	return &dispatch
}

func NewTargetPodControllerDispatch(vmCache cache.Store, restClient *rest.RESTClient, vmService services.VMService, clientset *kubernetes.Clientset, migrationQueue workqueue.RateLimitingInterface) kubecli.ControllerDispatch {
	dispatch := targetPodDispatch{
		vmCache:        vmCache,
		restClient:     restClient,
		vmService:      vmService,
		clientset:      clientset,
		migrationQueue: migrationQueue,
	}
	return &dispatch
}

type targetPodDispatch struct {
	vmCache        cache.Store
	restClient     *rest.RESTClient
	vmService      services.VMService
	clientset      *kubernetes.Clientset
	migrationQueue workqueue.RateLimitingInterface
}

type MigrationDispatch struct {
	restClient *rest.RESTClient
	vmService  services.VMService
	clientset  *kubernetes.Clientset
}

func (md *MigrationDispatch) Execute(store cache.Store, queue workqueue.RateLimitingInterface, key interface{}) {

	setMigrationPhase := func(migration *v1.Migration, phase v1.MigrationPhase) error {

		if migration.Status.Phase == phase {
			return nil
		}

		logger := logging.DefaultLogger().Object(migration)

		// Copy migration for future modifications
		migrationCopy, err := copy(migration)
		if err != nil {
			logger.Error().Reason(err).Msg("could not copy migration object")
			queue.AddRateLimited(key)
			return nil
		}

		migrationCopy.Status.Phase = phase
		// TODO indicate why it was set to failed
		err = md.vmService.UpdateMigration(migrationCopy)
		if err != nil {
			logger.Error().Reason(err).Msgf("updating migration state failed: %v ", err)
			queue.AddRateLimited(key)
			return err
		}
		queue.Forget(key)
		return nil
	}

	setMigrationFailed := func(mig *v1.Migration) {
		setMigrationPhase(mig, v1.MigrationFailed)
	}

	obj, exists, err := store.GetByKey(key.(string))
	if err != nil {
		queue.AddRateLimited(key)
		return
	}
	if !exists {
		queue.Forget(key)
		return
	}

	var migration *v1.Migration = obj.(*v1.Migration)
	logger := logging.DefaultLogger().Object(migration)

	vm, exists, err := md.vmService.FetchVM(migration.Spec.Selector.Name)
	if err != nil {
		logger.Error().Reason(err).Msgf("fetching the vm %s failed", migration.Spec.Selector.Name)
		queue.AddRateLimited(key)
		return
	}

	if !exists {
		logger.Info().Msgf("VM with name %s does not exist, marking migration as failed", migration.Spec.Selector.Name)
		setMigrationFailed(migration)
		return
	}

	switch migration.Status.Phase {
	case v1.MigrationUnknown:
		// Fetch vm which we want to migrate

		if vm.Status.Phase != v1.Running {
			logger.Error().Msgf("VM with name %s is in state %s, no migration possible. Marking migration as failed", vm.GetObjectMeta().GetName(), vm.Status.Phase)
			setMigrationFailed(migration)
			return
		}

		if err := mergeConstraints(migration, vm); err != nil {
			logger.Error().Reason(err).Msg("merging Migration and VM placement constraints failed.")
			queue.AddRateLimited(key)
			return
		}
		podList, err := md.vmService.GetRunningVMPods(vm)
		if err != nil {
			logger.Error().Reason(err).Msg("could not fetch a list of running VM target pods")
			queue.AddRateLimited(key)
			return
		}

		numOfPods, targetPod := investigateTargetPodSituation(migration, podList)

		if targetPod == nil {
			if numOfPods > 1 {
				logger.Error().Reason(err).Msg("another migration seems to be in progress, marking Migration as failed")
				// Another migration is currently going on
				setMigrationFailed(migration)
				return
			} else if numOfPods == 1 {
				// We need to start a migration target pod
				// TODO, this detection is not optimal, it can lead to strange situations
				err := md.vmService.CreateMigrationTargetPod(migration, vm)
				if err != nil {
					logger.Error().Reason(err).Msg("creating a migration target pod failed")
					queue.AddRateLimited(key)
					return
				}

			}
		} else {
			if targetPod.Status.Phase == k8sv1.PodFailed {
				setMigrationPhase(migration, v1.MigrationFailed)
				return
			}

			// Unlikely to hit this case, but prevents erroring out
			// if we re-enter this loop
			logger.Info().Msg("Migration appears to be set up, but was not set to InProgress.")
		}
		err = setMigrationPhase(migration, v1.MigrationInProgress)
		if err != nil {
			return
		}
	case v1.MigrationInProgress:
		podList, err := md.vmService.GetRunningVMPods(vm)
		if err != nil {
			logger.Error().Reason(err).Msg("could not fetch a list of running VM target pods")
			queue.AddRateLimited(key)
			return
		}

		_, targetPod := investigateTargetPodSituation(migration, podList)

		if targetPod == nil {
			setMigrationFailed(migration)
			return
		}

		switch targetPod.Status.Phase {
		case k8sv1.PodRunning:
			break
			//Figure out why. report.
		case k8sv1.PodSucceeded, k8sv1.PodFailed:
			setMigrationFailed(migration)
			return
		default:
			//Not requeuing, just not far enough along to proceed
			return
		}

		if vm.Status.MigrationNodeName != targetPod.Spec.NodeName {
			vm.Status.Phase = v1.Migrating
			vm.Status.MigrationNodeName = targetPod.Spec.NodeName
			if err = md.updateVm(vm); err != nil {
				queue.AddRateLimited(key)
			}
		}

		// Let's check if the job already exists, it can already exist in case we could not update the VM object in a previous run
		migrationPod, exists, err := md.vmService.GetMigrationJob(migration)

		if err != nil {
			logger.Error().Reason(err).Msg("Checking for an existing migration job failed.")
			queue.AddRateLimited(key)
			return
		}

		if !exists {
			sourceNode, err := md.clientset.CoreV1().Nodes().Get(vm.Status.NodeName, metav1.GetOptions{})
			if err != nil {
				logger.Error().Reason(err).Msgf("Fetching source node %s failed.", vm.Status.NodeName)
				queue.AddRateLimited(key)
				return
			}
			targetNode, err := md.clientset.CoreV1().Nodes().Get(vm.Status.MigrationNodeName, metav1.GetOptions{})
			if err != nil {
				logger.Error().Reason(err).Msgf("Fetching target node %s failed.", vm.Status.MigrationNodeName)
				queue.AddRateLimited(key)
				return
			}

			if err := md.vmService.StartMigration(migration, vm, sourceNode, targetNode, targetPod); err != nil {
				logger.Error().Reason(err).Msg("Starting the migration job failed.")
				queue.AddRateLimited(key)
				return
			}
			//TODO add a new state that more accurately reflects the process up the this point
			//and then use MigrationInProgress to accurately indicate the actual migration pod is running
			//setMigrationPhase(migration, v1.MigrationInProgress)

			return
		}

		switch migrationPod.Status.Phase {
		case k8sv1.PodFailed:
			setMigrationPhase(migration, v1.MigrationFailed)
			vm.Status.Phase = v1.Running
			vm.Status.MigrationNodeName = ""
			if err = md.updateVm(vm); err != nil {
				queue.AddRateLimited(key)
			}
			return
		case k8sv1.PodSucceeded:
			vm.Status.NodeName = targetPod.Spec.NodeName
			vm.Status.MigrationNodeName = ""
			vm.Status.Phase = v1.Running
			if err = md.updateVm(vm); err != nil {
				queue.AddRateLimited(key)
			}
			setMigrationPhase(migration, v1.MigrationSucceeded)
		}
	}
	queue.Forget(key)
	return
}
func (md *MigrationDispatch) updateVm(vmCopy *v1.VM) error {
	if _, err := md.vmService.PutVm(vmCopy); err != nil {
		logger := logging.DefaultLogger().Object(vmCopy)
		logger.V(3).Info().Msg("Enqueuing VM again.")
		return err
	}
	return nil
}

func copy(migration *v1.Migration) (*v1.Migration, error) {
	obj, err := kubeapi.Scheme.Copy(migration)
	if err != nil {
		return nil, err
	}
	return obj.(*v1.Migration), nil
}

// Returns the number of  running pods and if a pod for exactly that migration is currently running
func investigateTargetPodSituation(migration *v1.Migration, podList *k8sv1.PodList) (int, *k8sv1.Pod) {
	var targetPod *k8sv1.Pod = nil
	for _, pod := range podList.Items {
		if pod.Labels[v1.MigrationUIDLabel] == string(migration.GetObjectMeta().GetUID()) {
			targetPod = &pod
		}
	}
	return len(podList.Items), targetPod
}

func mergeConstraints(migration *v1.Migration, vm *v1.VM) error {

	merged := map[string]string{}
	for k, v := range vm.Spec.NodeSelector {
		merged[k] = v
	}
	conflicts := []string{}
	for k, v := range migration.Spec.NodeSelector {
		val, exists := vm.Spec.NodeSelector[k]
		if exists && val != v {
			conflicts = append(conflicts, k)
		} else {
			merged[k] = v
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("Conflicting node selectors: %v", conflicts)
	}
	vm.Spec.NodeSelector = merged
	return nil
}

func (pd *targetPodDispatch) Execute(podStore cache.Store, podQueue workqueue.RateLimitingInterface, key interface{}) {
	// Fetch the latest Vm state from cache
	obj, exists, err := podStore.GetByKey(key.(string))

	if err != nil {
		podQueue.AddRateLimited(key)
		return
	}

	if !exists {
		// Do nothing
		return
	}
	pod := obj.(*k8sv1.Pod)

	vmObj, exists, err := pd.vmCache.GetByKey(kubeapi.NamespaceDefault + "/" + pod.GetLabels()[corev1.DomainLabel])
	if err != nil {
		podQueue.AddRateLimited(key)
		return
	}
	if !exists {
		// Do nothing, the pod will timeout.
		return
	}
	vm := vmObj.(*corev1.VM)
	if vm.GetObjectMeta().GetUID() != types.UID(pod.GetLabels()[corev1.VMUIDLabel]) {
		// Obviously the pod of an outdated VM object, do nothing
		return
	}
	logger := logging.DefaultLogger()

	// Get associated migration
	obj, err = pd.restClient.Get().Resource("migrations").Namespace(k8sv1.NamespaceDefault).Name(pod.Labels[corev1.MigrationLabel]).Do().Get()
	if err != nil {
		logger.Error().Reason(err).Msgf("Fetching migration %s failed.", pod.Labels[corev1.MigrationLabel])
		podQueue.AddRateLimited(key)
		return
	}
	migration := obj.(*corev1.Migration)
	migrationKey, err := cache.MetaNamespaceKeyFunc(migration)
	if err == nil {
		pd.migrationQueue.Add(migrationKey)
	} else {
		logger.Error().Reason(err).Msgf("Updating migration podQueue failed.", pod.Labels[corev1.MigrationLabel])
		podQueue.AddRateLimited(key)
		return
	}
}
