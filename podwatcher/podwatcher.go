package podwatcher

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	config "github.com/SUSE/eirini-loggregator-bridge/config"
	. "github.com/SUSE/eirini-loggregator-bridge/logger"
	eirinix "github.com/SUSE/eirinix"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type PodWatcher struct {
	Config     config.ConfigType
	Containers ContainerList
	Manager    eirinix.Manager
}

type Container struct {
	killChannel   chan bool
	PodName       string
	Namespace     string
	Name          string
	PodUID        string
	UID           string
	InitContainer bool
	ContainerList *ContainerList
	State         *corev1.ContainerState
}

type ContainerList struct {
	PodWatcher *PodWatcher
	Containers map[string]*Container

	Tails sync.WaitGroup
}

func (cl *ContainerList) GetContainer(uid string) (*Container, bool) {
	c, ok := cl.Containers[uid]
	return c, ok
}

func (cl *ContainerList) AddContainer(c *Container) {
	c.ContainerList = cl
	cl.Containers[c.UID] = c

	LogDebug("Adding container ", c.UID)
	c.Read(&cl.Tails)
}

func (cl *ContainerList) RemoveContainer(uid string) error {
	_, ok := cl.GetContainer(uid)
	if ok {
		// TODO: Cleanup goroutine with killChannel
	}
	delete(cl.Containers, uid)
	return nil
}

// EnsureContainer make sure the container exists in the list and we are
// monitoring it.
func (cl ContainerList) EnsureContainer(c *Container) error {
	// TODO: implement this
	LogDebug(c.UID + ": ensuring container is monitored")

	if _, ok := cl.GetContainer(c.UID); !ok {
		cl.AddContainer(c)
	}
	return nil
}

func (cl ContainerList) RemovePODContainers(podUID string) error {
	// TODO: Fix this, and remove all containers belonging to a POD
	LogDebug("Removing POD's containers ", podUID)

	return nil
}

func (c *Container) Write(b []byte) (int, error) {
	/*
	   tlsConfig, err := loggregator.NewIngressTLSConfig(
	   		os.Getenv("LOGGREGATOR_CA_PATH"),
	   		os.Getenv("LOGGREGATOR_CERT_PATH"),
	   		os.Getenv("LOGGREGATOR_CERT_KEY_PATH"),
	   	)
	   	if err != nil {
	   		return 0, err
	   	}

	   	loggregatorClient, err := loggregator.NewIngressClient(
	   		tlsConfig,
	   		// Temporary make flushing more frequent to be able to debug
	   		loggregator.WithBatchFlushInterval(3*time.Second),
	   		loggregator.WithAddr(os.Getenv("LOGGREGATOR_ENDPOINT")),
	   	)

	   	if err != nil {
	   		return 0, err
	   	}
	*/
	LogDebug("POD OUTPUT: " + string(b))

	//loggregatorClient.Emit(lw.Envelope(b))

	return len(b), nil
}

func (c *Container) Read(wg *sync.WaitGroup) {
	wg.Add(1)
	go func(c *Container, w *sync.WaitGroup) {
		defer wg.Done()
		err := c.Tail()
		if err != nil {
			LogError("Error: ", err.Error())
		}
	}(c, wg)
}

// Tail connects to the Kube
func (c Container) Tail() error {
	manager := c.ContainerList.PodWatcher.Manager
	config, err := manager.GetKubeConnection()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "Failed to create a kube client")
	}
	req := kubeClient.CoreV1().RESTClient().Get().
		Namespace(c.ContainerList.PodWatcher.Config.Namespace).
		Name(c.PodName).
		Resource("pods").
		SubResource("log").
		Param("follow", strconv.FormatBool(true)).
		Param("container", c.Name).
		Param("previous", strconv.FormatBool(false)).
		Param("timestamps", strconv.FormatBool(false))
	readCloser, err := req.Stream()
	if err != nil {
		return err
	}

	defer readCloser.Close()
	_, err = io.Copy(&c, readCloser)
	if err != nil {
		return err
	}

	return nil
}

func (c *Container) generateUID() {
	c.UID = fmt.Sprintf("%s-%s", string(c.PodUID), c.Name)
}

func (c *Container) findState(containerStatuses []corev1.ContainerStatus) {
	for _, status := range containerStatuses {
		if status.Name == c.Name {
			c.State = &status.State
		}
	}
}

// Cleanup removes containers from the containerlist if they don't exist in the given
// map. This should be used to remove leftover containers from our containerlist
// when they disappear from the pod.
func (cl *ContainerList) Cleanup(existingContainers map[string]*Container) {
	for _, c := range cl.Containers {
		if _, ok := existingContainers[c.UID]; !ok {
			cl.RemoveContainer(c.UID)
		}
	}
}

// UpdateContainer decides whether a container should be added, left alone
// or removed from the container list. It does that but checking the state of
// of the container.
func (cl *ContainerList) UpdateContainer(c *Container) error {
	LogDebug("I'm checking container with state", c.State)
	if c.State != nil && c.State.Running != nil {
		LogDebug("I'm adding container", c)
		cl.EnsureContainer(c)
	} else {
		err := cl.RemoveContainer(c.UID)
		if err != nil {
			return err
		}
	}
	return nil
}

func ExtractContainersFromPod(pod *corev1.Pod) map[string]*Container {
	result := map[string]*Container{}
	// NOTE: The order of the lists matter!
	for i, clist := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		cstatuses := [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses}
		for _, c := range clist {
			container := &Container{
				Name:          c.Name,
				PodName:       pod.Name,
				PodUID:        string(pod.UID),
				Namespace:     pod.Namespace,
				killChannel:   make(chan bool),
				InitContainer: (i == 0),
			}
			container.generateUID()
			container.findState(cstatuses[i])
			result[container.UID] = container
		}
	}
	return result
}

// EnsurePodStatus handles a pod event by adding or removing container tailing
// goroutines. Every running container in the monitored namespace has its own
// goroutine that reads its log stream. When a container is stopped we stop
// the relevant gorouting (if it is still running, it could already be stopped
// because of an error).
func (cl *ContainerList) EnsurePodStatus(pod *corev1.Pod) error {
	podContainers := ExtractContainersFromPod(pod)

	for _, c := range podContainers {
		cl.UpdateContainer(c)
	}

	cl.Cleanup(podContainers)

	cl.Tails.Wait()

	return nil
}

func NewPodWatcher(config config.ConfigType, manager eirinix.Manager) eirinix.Watcher {
	pw := &PodWatcher{
		Config:  config,
		Manager: manager}
	// We need a way to go up the hierarchy (e.g. to access the Manager from the Container):
	// Manager -> PodWatcher -> ContainerList -> Container
	pw.Containers = ContainerList{PodWatcher: pw, Containers: map[string]*Container{}}

	return pw
}

func (pw *PodWatcher) Handle(manager eirinix.Manager, e watch.Event) {
	manager.GetLogger().Debug("Received event: ", e)
	if e.Object == nil {
		// Closed because of error
		// TODO: Handle errors ( maybe kill the whole application )
		// because it is going to run in goroutines, and we can't
		// just return gracefully and panicking the whole
		return
	}

	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		manager.GetLogger().Error("Received non-pod object in watcher channel")
		return
	}

	pw.Containers.EnsurePodStatus(pod)

	// TODO:
	// - Consume a kubeclient ✓ -> moved to eirinix
	// - Create kube watcher ✓ -> moved to eirinix
	// - Select on channels and handle events and spin up go routines for the new pod
	//   Or stop goroutine for removed pods
	//   Those goroutines read the logs  of the pod from the kube api and simply  writes metadata to a channel.
	// - Then we have one or more reader instances that consumes the channel, converting metadata to loggregator envelopes and streams that to loggregator
}
