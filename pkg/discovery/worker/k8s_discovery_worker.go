package worker

import (
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	client "k8s.io/client-go/kubernetes"
	api "k8s.io/client-go/pkg/api/v1"

	"github.com/turbonomic/kubeturbo/pkg/discovery/dtofactory"
	"github.com/turbonomic/kubeturbo/pkg/discovery/metrics"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring"
	"github.com/turbonomic/kubeturbo/pkg/discovery/monitoring/types"
	"github.com/turbonomic/kubeturbo/pkg/discovery/probe/stitching"
	"github.com/turbonomic/kubeturbo/pkg/discovery/task"

	"github.com/turbonomic/turbo-go-sdk/pkg/proto"

	"github.com/golang/glog"
	"github.com/pborman/uuid"
)

type k8sDiscoveryWorkerConfig struct {
	// TODO, once we know what are the required method, we can use a clusterAccessor.
	kubeClient *client.Clientset

	// a collection of all configs for building different monitoring clients.
	// key: monitor type; value: monitor worker config.
	monitoringSourceConfigs map[types.MonitorType][]monitoring.MonitorWorkerConfig

	stitchingPropertyType stitching.StitchingPropertyType
}

func NewK8sDiscoveryWorkerConfig(kubeClient *client.Clientset, sType stitching.StitchingPropertyType) *k8sDiscoveryWorkerConfig {
	return &k8sDiscoveryWorkerConfig{
		kubeClient:              kubeClient,
		stitchingPropertyType:   sType,
		monitoringSourceConfigs: make(map[types.MonitorType][]monitoring.MonitorWorkerConfig),
	}
}

// Add new monitoring worker config to the discovery worker config.
func (c *k8sDiscoveryWorkerConfig) WithMonitoringWorkerConfig(config monitoring.MonitorWorkerConfig) *k8sDiscoveryWorkerConfig {
	monitorType := config.GetMonitorType()
	configs, exist := c.monitoringSourceConfigs[monitorType]
	if !exist {
		configs = []monitoring.MonitorWorkerConfig{}
	}

	configs = append(configs, config)
	c.monitoringSourceConfigs[monitorType] = configs

	return c
}

// k8sDiscoveryWorker receives a discovery task from dispatcher(DiscoveryClient). Then ask available monitoring workers
// to scrape metrics source and get topology information. Finally it builds entityDTOs and send back to DiscoveryClient.
type k8sDiscoveryWorker struct {
	// A UID of a discovery worker.
	id string

	// config
	config *k8sDiscoveryWorkerConfig

	// a collection of all monitoring worker of different types.
	// key: monitoring types; value: monitoring worker instance.
	monitoringWorker map[types.MonitorType][]monitoring.MonitoringWorker

	// sink is a central place to store all the monitored data.
	sink *metrics.EntityMetricSink

	taskChan chan *task.Task
}

func NewK8sDiscoveryWorker(config *k8sDiscoveryWorkerConfig) (*k8sDiscoveryWorker, error) {
	id := uuid.NewUUID().String()

	if len(config.monitoringSourceConfigs) == 0 {
		return nil, errors.New("No monitoring source config found in config.")
	}

	// Build all the monitoring worker based on configs.
	monitoringWorkerMap := make(map[types.MonitorType][]monitoring.MonitoringWorker)
	for monitorType, configList := range config.monitoringSourceConfigs {
		monitorList, exist := monitoringWorkerMap[monitorType]
		if !exist {
			monitorList = []monitoring.MonitoringWorker{}
		}
		for _, config := range configList {
			monitoringWorker, err := monitoring.BuildMonitorWorker(config.GetMonitoringSource(), config)
			if err != nil {
				// TODO return?
				glog.Errorf("Failed to build monitoring worker configuration: %v", err)
				continue
			}
			monitorList = append(monitorList, monitoringWorker)
		}
		monitoringWorkerMap[monitorType] = monitorList
	}

	return &k8sDiscoveryWorker{
		id:               id,
		config:           config,
		monitoringWorker: monitoringWorkerMap,
		sink:             metrics.NewEntityMetricSink(),

		taskChan: make(chan *task.Task),
	}, nil
}

func (worker *k8sDiscoveryWorker) RegisterAndRun(dispatcher *Dispatcher, collector *ResultCollector) {
	worker.registerToDispatcher(dispatcher)
	//worker.registerToResultCollector(collector)
	for {
		select {
		case currTask := <-worker.taskChan:
			glog.V(3).Infof("Worker %s has received a discovery task.", worker.id)
			result := worker.executeTask(currTask)
			collector.ResultPool() <- result
			glog.V(3).Infof("Worker %s has finished the discovery task.", worker.id)

			worker.registerToDispatcher(dispatcher)

		}
	}
}

func (worker *k8sDiscoveryWorker) registerToDispatcher(dispatcher *Dispatcher) {
	glog.V(3).Infof("Register current worker %s to dispatcher", worker.id)
	dispatcher.WorkerPool() <- worker.taskChan
}

// Worker start to working on task.
func (worker *k8sDiscoveryWorker) executeTask(currTask *task.Task) *task.TaskResult {
	if currTask == nil {
		err := errors.New("No task has been assigned to current worker.")
		glog.Errorf("%s", err)
		return task.NewTaskResult(worker.id, task.TaskFailed).WithErr(err)
	}

	// Resource monitoring
	resourceMonitorTask := currTask
	if resourceMonitoringWorkers, exist := worker.monitoringWorker[types.ResourceMonitor]; exist {
		for _, rmWorker := range resourceMonitoringWorkers {
			glog.V(2).Infof("A %s monitoring worker is invoked.", rmWorker.GetMonitoringSource())
			// Assign task to monitoring worker.
			rmWorker.ReceiveTask(resourceMonitorTask)
			monitoringSink := rmWorker.Do()
			// Don't do any filtering
			worker.sink.MergeSink(monitoringSink, nil)
		}
	}

	// Topology monitoring
	clusterMonitorTask := currTask
	if resourceMonitoringWorkers, exist := worker.monitoringWorker[types.StateMonitor]; exist {
		for _, smWorker := range resourceMonitoringWorkers {
			// Assign task to monitoring worker.
			smWorker.ReceiveTask(clusterMonitorTask)
			monitoringSink := smWorker.Do()
			// Don't do any filtering
			worker.sink.MergeSink(monitoringSink, nil)
		}
	}

	var discoveryResult []*proto.EntityDTO
	// Build EntityDTO
	// node
	stitchingManager := stitching.NewStitchingManager(worker.config.stitchingPropertyType)

	var nodes []runtime.Object
	nodeNameUIDMap := make(map[string]string)
	for _, n := range currTask.NodeList() {
		node := n
		nodes = append(nodes, node)
		nodeNameUIDMap[node.Name] = string(node.UID)
		stitchingManager.StoreStitchingValue(node)
	}

	nodeEntityDTOBuilder := dtofactory.NewNodeEntityDTOBuilder(worker.sink, stitchingManager)
	nodeEntityDTOs, err := nodeEntityDTOBuilder.BuildEntityDTOs(nodes)
	if err != nil {
		glog.Errorf("Error while creating node entityDTOs: %v", err)
		// TODO Node discovery fails, directly return?
	}
	glog.V(2).Infof("Build %d node entityDTOs.", len(nodeEntityDTOs))
	discoveryResult = append(discoveryResult, nodeEntityDTOs...)

	// pod
	pods := worker.findPodsRunningInTaskNodes(currTask.NodeList())
	podEntityDTOBuilder := dtofactory.NewPodEntityDTOBuilder(worker.sink, stitchingManager, nodeNameUIDMap)
	podEntityDTOs, err := podEntityDTOBuilder.BuildEntityDTOs(pods)
	if err != nil {
		glog.Errorf("Error while creating pod entityDTOs: %v", err)
		// TODO Pod discovery fails, directly return?
	}
	discoveryResult = append(discoveryResult, podEntityDTOs...)
	glog.V(2).Infof("Build %d pod entityDTOs.", len(podEntityDTOs))

	// application
	applicationEntityDTOBuilder := dtofactory.NewApplicationEntityDTOBuilder(worker.sink)
	appEntityDTOs, err := applicationEntityDTOBuilder.BuildEntityDTOs(pods)
	if err != nil {
		glog.Errorf("Error while creating application entityDTOs: %v", err)
		// TODO Application discovery fails, return?
	}
	discoveryResult = append(discoveryResult, appEntityDTOs...)
	glog.V(2).Infof("Build %d application entityDTOs.", len(appEntityDTOs))

	// Send result
	glog.V(3).Infof("Discovery result of worker %s is: %++v", worker.id, discoveryResult)

	result := task.NewTaskResult(worker.id, task.TaskSucceeded).WithContent(discoveryResult)
	return result
}

// Discover pods running on nodes specified in task.
func (worker *k8sDiscoveryWorker) findPodsRunningInTaskNodes(nodes []*api.Node) []runtime.Object {
	var podList []runtime.Object
	for _, node := range nodes {
		nodeNonTerminatedPodsList, err := worker.findRunningPods(node.Name)
		if err != nil {
			glog.Errorf("Failed to find non-ternimated pods in %s", node.Name)
			continue
		}
		for _, p := range nodeNonTerminatedPodsList.Items {
			pod := p
			podList = append(podList, &pod)
		}
	}
	return podList
}

func (worker *k8sDiscoveryWorker) findRunningPods(nodeName string) (*api.PodList, error) {
	fieldSelector, err := fields.ParseSelector("spec.nodeName=" + nodeName + ",status.phase=" +
		string(api.PodRunning))
	if err != nil {
		return nil, err
	}
	return worker.config.kubeClient.CoreV1().Pods(api.NamespaceAll).List(metav1.ListOptions{FieldSelector: fieldSelector.String()})
}
