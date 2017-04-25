package watch

import (
	"github.com/facebookgo/inject"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/util/workqueue"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/logging"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

var _ = Describe("Pod", func() {
	var server *ghttp.Server

	var vmCache cache.Store
	var podCache cache.Store
	var vmService services.VMService
	var podDispatch kubecli.ControllerDispatch
	var migrationPodDispatch kubecli.ControllerDispatch
	var migrationQueue workqueue.RateLimitingInterface

	logging.DefaultLogger().SetIOWriter(GinkgoWriter)

	BeforeEach(func() {
		server = ghttp.NewServer()
		restClient, err := kubecli.GetRESTClientFromFlags(server.URL(), "")
		Expect(err).To(Not(HaveOccurred()))
		vmCache = cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, nil)
		podCache = cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, nil)

		var g inject.Graph
		vmService = services.NewVMService()
		server = ghttp.NewServer()
		config := rest.Config{}
		config.Host = server.URL()
		clientSet, _ := kubernetes.NewForConfig(&config)
		templateService, _ := services.NewTemplateService("kubevirt/virt-launcher")
		restClient, _ = kubecli.GetRESTClientFromFlags(server.URL(), "")
		migrationQueue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

		g.Provide(
			&inject.Object{Value: restClient},
			&inject.Object{Value: clientSet},
			&inject.Object{Value: vmService},
			&inject.Object{Value: templateService},
		)
		g.Populate()

		podDispatch = NewPodControllerDispatch(vmCache, restClient, vmService, clientSet)
		migrationPodDispatch = NewMigrationPodControllerDispatch(vmCache, restClient, vmService, clientSet, migrationQueue)
	})

})
