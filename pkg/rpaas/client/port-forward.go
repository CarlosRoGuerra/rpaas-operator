package client

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/rpaas-operator/pkg/observability"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubectl/pkg/scheme"
	sigsk8sconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

type labelsFlags map[string]string

func (l *labelsFlags) String() string {
	return fmt.Sprintf("%v", *l)
}
func (l *labelsFlags) Set(value string) error {
	label := strings.SplitN(value, "=", 2)
	if len(label) != 2 {
		return errors.New("labels must include equal sign")
	}
	(*l)[label[0]] = label[1]
	return nil
}

type PortForward struct {
	Config          *rest.Config
	Clientset       kubernetes.Interface
	Name            string
	Labels          metav1.LabelSelector
	DestinationPort int
	ListenPort      int
	Namespace       string
	StopChan        chan struct{}
	ReadyChan       chan struct{}
}

func NewPortForwarder(name string, labels metav1.LabelSelector, port int, namespace string) (*PortForward, error) {
	pf := &PortForward{
		Name:            name,
		DestinationPort: port,
		Namespace:       namespace,
	}

	var err error
	restConfig, err := sigsk8sconfig.GetConfig()
	if err != nil {
		return nil, err
	}
	restConfig.WrapTransport = observability.OpentracingTransport

	pf.Config = restConfig

	pf.Clientset, err = kubernetes.NewForConfig(pf.Config)
	if err != nil {
		return pf, errors.Wrap(err, "Could not create kubernetes client")
	}
	return pf, nil
}

// Start a port forward to a pod - blocks until the tunnel is ready for use.
func (p *PortForward) Start(ctx context.Context) error {
	p.StopChan = make(chan struct{}, 1)
	readyChan := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	listenPort, err := p.getListenPort()
	if err != nil {
		return errors.Wrap(err, "Could not find a port to bind to")
	}
	dialer, err := p.dialer(ctx)
	if err != nil {
		return errors.Wrap(err, "Could not create a dialer")
	}

	ports := []string{
		fmt.Sprintf("%d:%d", listenPort, 80),
	}
	discard := ioutil.Discard
	pf, err := portforward.New(dialer, ports, p.StopChan, readyChan, discard, discard)
	if err != nil {
		return errors.Wrap(err, "Could not port forward into pod")
	}

	go func() {
		errChan <- pf.ForwardPorts()
	}()

	select {
	case err = <-errChan:
		return errors.Wrap(err, "Could not create port forward")
	case <-readyChan:
		return nil
	}

}

// Stop a port forward.
func (p *PortForward) Stop() {
	p.StopChan <- struct{}{}
}

// Returns the port that the port forward should listen on.
// If ListenPort is set, then it returns ListenPort.
// Otherwise, it will call getFreePort() to find an open port.
func (p *PortForward) getListenPort() (int, error) {
	var err error

	if p.ListenPort == 0 {
		p.ListenPort, err = p.getFreePort()
	}

	return p.ListenPort, err
}

// Get a free port on the system by binding to port 0, checking
// the bound port number, and then closing the socket.
func (p *PortForward) getFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := listener.Addr().(*net.TCPAddr).Port
	err = listener.Close()
	if err != nil {
		return 0, err
	}

	return port, nil
}

// Create an httpstream.Dialer for use with portforward.New
func (p *PortForward) dialer(ctx context.Context) (httpstream.Dialer, error) {
	pod, err := p.getPodName(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "Could not get pod name")
	}
	url := p.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(p.Namespace).
		Name(pod).
		SubResource("portforward").VersionedParams(&v1.PodExecOptions{
		Container: "nginx",
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec).URL()

	transport, upgrader, err := spdy.RoundTripperFor(p.Config)
	if err != nil {
		return nil, errors.Wrap(err, "Could not create round tripper")
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	return dialer, nil
}

// Gets the pod name to port forward to, if Name is set, Name is returned. Otherwise,
// it will call findPodByLabels().
func (p *PortForward) getPodName(ctx context.Context) (string, error) {
	var err error
	if p.Name == "" {
		p.Name, err = p.findPodByLabels(ctx)
	}
	return p.Name, err
}

func (p *PortForward) findPodByLabels(ctx context.Context) (string, error) {
	if len(p.Labels.MatchLabels) == 0 && len(p.Labels.MatchExpressions) == 0 {
		return "", errors.New("No pod labels specified")
	}

	pods, err := p.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(&p.Labels),
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(v1.PodRunning)).String(),
	})

	if err != nil {
		return "", errors.Wrap(err, "Listing pods in kubernetes")
	}

	formatted := metav1.FormatLabelSelector(&p.Labels)

	if len(pods.Items) == 0 {
		return "", errors.New(fmt.Sprintf("Could not find running pod for selector: labels \"%s\"", formatted))
	}

	if len(pods.Items) != 1 {
		return "", errors.New(fmt.Sprintf("Ambiguous pod: found more than one pod for selector: labels \"%s\"", formatted))
	}

	return pods.Items[0].ObjectMeta.Name, nil
}

func (c *client) StartPortForward(ctx context.Context, args PortForwardArgs) (*PortForward, error) {
	var err error
	var Namespace, Pod string

	var ListenPort, Port int

	labels := labelsFlags{}

	flag.Var(&labels, "label", "")
	flag.IntVar(&ListenPort, "listen", ListenPort, "port to bind")
	flag.IntVar(&Port, "Port", args.DestinationPort, "port to forward")
	flag.StringVar(&Pod, "pod", args.Pod, "pod name")
	flag.StringVar(&Namespace, "namespace", args.Instance, "namespacepod look for")
	flag.Parse()

	pf, err := NewPortForwarder(args.Pod, metav1.LabelSelector{MatchLabels: labels}, args.DestinationPort, args.Instance)
	if err != nil {
		return pf, err
	}

	pf.ListenPort = args.ListenPort
	err = pf.Start(context.TODO())
	if err != nil {
		log.Fatal("Error starting port forward:", err)
	}
	log.Printf("Started tunnel on %d\n", pf.ListenPort)
	time.Sleep(60 * time.Second)

	return pf, err
}
