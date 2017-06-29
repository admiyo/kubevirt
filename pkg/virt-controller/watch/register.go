package watch

import (
	"flag"
	"net/http"
	"reflect"
	"strconv"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	kubeapi "k8s.io/client-go/pkg/api"
	k8sv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kubev1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/dependencies"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/logging"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

const vms = "vms"

var (
	FS dependencies.FactorySet = dependencies.NewFactorySet()
	CC dependencies.ComponentCache

	launcherImage     string = ""
	migratorImage     string = ""
	Host              string
	Port              int
	common_registered bool         = false
	live_registered   bool         = false
	ClientsetType     reflect.Type = reflect.TypeOf((*kubernetes.Clientset)(nil))
)

type RateLimitingInterfaceStruct struct {
	workqueue.RateLimitingInterface
}
type IndexerStruct struct {
	cache.Indexer
}

func RegisterLive() {
	if live_registered {
		return
	}
	live_registered = true
	registerCommon()
	FS.Register(ClientsetType, createClientSet)
	FS.Register(reflect.TypeOf((*http.Server)(nil)), createHttpServer)
	FS.Register(reflect.TypeOf((*rest.RESTClient)(nil)), createRestClient)
	FS.Register(reflect.TypeOf((*VMController)(nil)), createVMController)

}

func registerCommon() {
	if common_registered {
		return
	}
	common_registered = true

	FS.Register(reflect.TypeOf((*StoreAndInformer)(nil)), createStoreAndInformer)
	FS.Register(reflect.TypeOf((*MigrationController)(nil)), createMigrationController)

	FS.Register(reflect.TypeOf((*VMServiceStruct)(nil)), createVMService)
	FS.Register(reflect.TypeOf((*TemplateServiceStruct)(nil)), createTemplateService)

	FS.RegisterFactory(reflect.TypeOf((*IndexerStruct)(nil)), "migration", createCache)
	FS.RegisterFactory(reflect.TypeOf((*RateLimitingInterfaceStruct)(nil)), "migration", createQueue)
	FS.RegisterFactory(reflect.TypeOf((*cache.ListWatch)(nil)), "migration", createListWatch)

	FS.RegisterFactory(reflect.TypeOf((*IndexerStruct)(nil)), "vms", createCache)
	FS.RegisterFactory(reflect.TypeOf((*RateLimitingInterfaceStruct)(nil)), "vms", createQueue)
	FS.RegisterFactory(reflect.TypeOf((*cache.ListWatch)(nil)), "vms", createListWatch)

	flag.StringVar(&migratorImage, "migrator-image", "virt-handler", "Container which orchestrates a VM migration")
	flag.StringVar(&launcherImage, "launcher-image", "virt-launcher", "Shim container for containerized VMs")
	flag.StringVar(&Host, "listen", "0.0.0.0", "Address and Port where to listen on")
	flag.IntVar(&Port, "port", 8182, "Port to listen on")

	CC = dependencies.NewComponentCache(FS)

	flag.Parse()
}

type VMServiceStruct struct {
	services.VMService
}

//TODO Wrap with a structure
func createVMService(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	return &VMServiceStruct{
		services.NewVMService(
			GetClientSet(CC),
			GetRestClient(CC),
			*GetTemplateService(CC))}, nil
}

func createRestClient(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	return kubecli.GetRESTClient()
}

func createClientSet(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	return kubecli.Get()
}

type TemplateServiceStruct struct {
	services.TemplateService
}

func createTemplateService(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	ts, err := services.NewTemplateService(launcherImage, migratorImage)
	return &TemplateServiceStruct{
		ts,
	}, err
}

func createVMController(cc dependencies.ComponentCache, _ string) (interface{}, error) {

	lw := cache.NewListWatchFromClient(GetRestClient(cc), "vms", kubeapi.NamespaceDefault, fields.Everything())
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	indexer, informer := cache.NewIndexerInformer(lw, &kubev1.VM{}, 0, kubecli.NewResourceEventHandlerFuncsForWorkqueue(queue), cache.Indexers{})
	return &VMController{
		restClient: GetRestClient(cc),
		vmService:  *GetVMService(cc),
		queue:      queue,
		store:      indexer,
		informer:   informer,
		recorder:   nil,
		clientset:  GetClientSet(cc),
	}, nil

}

func createMigrationController(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	return NewMigrationController(GetVMService(cc), GetRestClient(cc), GetClientSet(cc)), nil
}

func createCache(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	migrationCache := &IndexerStruct{cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, nil)}
	return migrationCache, nil
}

func createQueue(cc dependencies.ComponentCache, _ string) (interface{}, error) {

	migrationQueue := RateLimitingInterfaceStruct{workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())}
	return &migrationQueue, nil
}

func createListWatch(cc dependencies.ComponentCache, which string) (interface{}, error) {
	return cache.NewListWatchFromClient(GetRestClient(cc), which, k8sv1.NamespaceDefault, fields.Everything()), nil
}

type StoreAndInformer struct {
	Store    cache.Indexer
	Informer cache.Controller
}

func createStoreAndInformer(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	lw := GetListWatch(cc, "migration")
	queue := GetQueue(cc, "migration")
	store, informer := cache.NewIndexerInformer(lw, &kubev1.Migration{}, 0, kubecli.NewResourceEventHandlerFuncsForWorkqueue(queue), cache.Indexers{})
	return StoreAndInformer{
		store,
		informer,
	}, nil

}

func createHttpServer(cc dependencies.ComponentCache, _ string) (interface{}, error) {
	logger := logging.DefaultLogger()
	httpLogger := logger.With("service", "http")
	httpLogger.Info().Log("action", "listening", "interface", Host, "port", Port)
	Address := Host + ":" + strconv.Itoa(Port)
	server := &http.Server{Addr: Address, Handler: nil}
	return server, nil
}

// Accessor functions below

func GetStoreAndInformer(cc dependencies.ComponentCache) *StoreAndInformer {
	return cc.FetchComponent(reflect.TypeOf((*StoreAndInformer)(nil)), "migration").(*StoreAndInformer)
}

func GetListWatch(cc dependencies.ComponentCache, which string) *cache.ListWatch {
	return cc.FetchComponent(reflect.TypeOf((*cache.ListWatch)(nil)), which).(*cache.ListWatch)
}

func GetQueue(cc dependencies.ComponentCache, which string) *RateLimitingInterfaceStruct {
	return cc.FetchComponent(reflect.TypeOf((*RateLimitingInterfaceStruct)(nil)), which).(*RateLimitingInterfaceStruct)
}

func GetCache(cc dependencies.ComponentCache, which string) *IndexerStruct {
	return cc.FetchComponent(reflect.TypeOf((*IndexerStruct)(nil)), which).(*IndexerStruct)
}

func GetClientSet(cc dependencies.ComponentCache) *kubernetes.Clientset {
	return cc.Fetch(ClientsetType).(*kubernetes.Clientset)
}

func GetRestClient(cc dependencies.ComponentCache) *rest.RESTClient {
	return cc.Fetch(reflect.TypeOf((*rest.RESTClient)(nil))).(*rest.RESTClient)
}

func GetTemplateService(cc dependencies.ComponentCache) *TemplateServiceStruct {
	return CC.Fetch(reflect.TypeOf((*TemplateServiceStruct)(nil))).(*TemplateServiceStruct)
}

func GetVMService(cc dependencies.ComponentCache) *VMServiceStruct {
	return cc.Fetch(reflect.TypeOf((*VMServiceStruct)(nil))).(*VMServiceStruct)
}

func GetVMController(cc dependencies.ComponentCache) *VMController {
	return (cc.Fetch(reflect.TypeOf((*VMController)(nil)))).(*VMController)
}

func GetMigrationController(cc dependencies.ComponentCache) *MigrationController {
	return cc.Fetch(reflect.TypeOf((*MigrationController)(nil))).(*MigrationController)
}

func GetHttpServer(cc dependencies.ComponentCache) *http.Server {
	return cc.Fetch(reflect.TypeOf((*http.Server)(nil))).(*http.Server)
}
