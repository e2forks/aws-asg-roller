package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	drain "github.com/openshift/kubernetes-drain"
	"github.com/rs/zerolog/log"
)

type K8sAwsNode struct {
	id       string
	hostname string
}

type asgState struct {
	ids         []string
	hostnames   []string
	hostnameMap map[string]string
}

// AsgGroup describes an AWS autoscaling group than needs to be managed by
// aws-asg-roller.
type AsgGroup struct {
	asg                 *autoscaling.Group
	originalNumInstance int64
	readinessHandler    readiness
	state               asgState
	nodes               []K8sAwsNode
}

// NewAsgGroups creates the state for the asg to be managed.
func NewAsgGroups(asgs []*autoscaling.Group, readinessHandler readiness) []AsgGroup {
	groups := make([]AsgGroup, 0, len(asgs))
	for _, asg := range asgs {
		groups = append(groups, NewAsgGroup(asg, readinessHandler))
	}
	return groups
}

// NewAsgGroup creates a new AsgGroup instance from an asg.
func NewAsgGroup(asg *autoscaling.Group, readinessHandler readiness) AsgGroup {
	group := AsgGroup{
		asg:                 asg,
		originalNumInstance: *asg.DesiredCapacity,
		readinessHandler:    readinessHandler,
		state:               asgState{ids: []string{}, hostnames: []string{}, hostnameMap: map[string]string{}},
	}
	return group
}

type AsgController struct {
	clientset        *kubernetes.Clientset
	ec2Svc           ec2iface.EC2API
	ignoreDaemonSets bool
	deleteLocalData  bool
}

func NewAsgController() (AsgController, error) {
	clientset, err := kubeGetClientset()
	if err != nil {
		return AsgController{}, err
	}
	return AsgController{clientset: clientset}, nil
}

// updateState updates the asg group internal state.
func (c *AsgController) updateAsgState(a *AsgGroup) error {

	ids := mapInstancesIds(a.asg.Instances)
	hostnameNotKnown := make([]string, 0)
	hostnameMap := map[string]string{}

	// ids that need to query the hostname
	for _, id := range ids {
		if hostname, ok := a.state.hostnameMap[id]; ok {
			hostnameMap[id] = hostname
		} else {
			hostnameNotKnown = append(hostnameNotKnown, id)
		}
	}
	// get missing hostnames
	hostnames, err := awsGetHostnames(c.ec2Svc, hostnameNotKnown)
	if err != nil {
		return fmt.Errorf("Unable to get aws hostnames for ids %v: %v", ids, err)
	}
	// update map
	for i, id := range hostnameNotKnown {
		hostnameMap[id] = hostnames[i]
	}

	// update state
	size := len(ids)
	a.state.hostnames = make([]string, size, size)
	for i, id := range ids {
		a.state.hostnames[i] = hostnameMap[id]
	}
	a.state.ids = ids
	a.state.hostnameMap = hostnameMap

	nodes := make([]K8sAwsNode, 0, size)
	for id, hostname := range hostnameMap {
		nodes = append(nodes, K8sAwsNode{id: id, hostname: hostname})
	}
	a.nodes = nodes

	return nil
}

func (c *AsgController) inferNodeLabel(asg AsgGroup) {
	nodes := c.clientset.CoreV1().Nodes()
	node, err := nodes.Get(asg.state.hostnames[0], v1.GetOptions{})
}

func (c *AsgController) getUnreadyCount(asg AsgGroup) (int, error) {
	hostHash := map[string]bool{}
	for _, h := range asg.state.hostnames {
		hostHash[h] = true
	}
	/*
		in AWS, the `name` of the node *always* is the internal private DNS name
		you can get a node by name by doing Nodes().Get(name)
		In other words the `name` of the node is set independently and does not care what
		the kubelet had for --hostname-override.
		However, if you want multiple nodes, you need to use the `List()` interface.
		This interface does not accept multiple hostnames. It lists everything, subject only to a filter
		The filter, however, can filter only on labels, and not on the name.
		We _should_ be able to just filter on kubernetes.io/hostname label, but this label *does*
		respect --hostname-override, which we do not know if it is set or not. Oops.
		This, for now, we are stuck doing multiple Get(), one for each hostname, or doing a List() of all nodes
	*/
	nodes, err := c.clientset.CoreV1().Nodes().List(v1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("Unexpected error getting nodes for cluster: %v", err)
	}
	unReadyCount := 0
	for _, n := range nodes.Items {
		// first make sure that this is one of the new nodes we care about
		if _, ok := hostHash[n.ObjectMeta.Name]; !ok {
			continue
		}
		// next check its status
		conditions := n.Status.Conditions
		if conditions[len(conditions)-1].Type != corev1.NodeReady {
			unReadyCount++
		}
	}
	return unReadyCount, nil
}

func (c *AsgController) prepareTermination(hostnames []string) error {
	// get the node reference - first need the hostname
	var (
		node     *corev1.Node
		hostname string
		err      error
	)
	for _, h := range hostnames {
		node, err = c.clientset.CoreV1().Nodes().Get(h, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("Unexpected error getting kubernetes node %s: %v", hostname, err)
		}
		// set options and drain nodes
		err = drain.Drain(c.clientset, []*corev1.Node{node}, &drain.DrainOptions{
			IgnoreDaemonsets:   c.ignoreDaemonSets,
			GracePeriodSeconds: -1,
			Force:              true,
			DeleteLocalData:    c.deleteLocalData,
		})
		if err != nil {
			return fmt.Errorf("Unexpected error draining kubernetes node %s: %v", hostname, err)
		}
	}
	return nil
}

func (c *AsgController) isNodeReady(n K8sAwsNode) (bool, error) {
	nodes := c.clientset.CoreV1().Nodes()
	node, err := nodes.Get(n.hostname, v1.GetOptions{})
	if err != nil {
		return false, err
	}
	conditions := node.Status.Conditions
	return conditions[len(conditions)-1].Type == corev1.NodeReady, nil
}

func (a *AsgGroup) terminationCandidate() string {

}

// requestNewInstance increases the number of desired instance of the asg by 1.
func (a *AsgGroup) requestNewInstance(svc autoscalingiface.AutoScalingAPI) error {
	return setAsgDesired(svc, a.asg, *a.asg.DesiredCapacity+1)
}

func (a *AsgGroup) prepareTermination() error {
	unReadyCount, err := a.readinessHandler.getUnreadyCount(a.state.hostnames, a.state.ids)
	if err != nil {
		return err
	}
	// do not terminate any node until asg is at steady state
	if unReadyCount > 0 {
		return fmt.Errorf("[%s] Nodes not ready: %d", *a.asg.AutoScalingGroupName, unReadyCount)
	}

}

func (a *AsgGroup) ReadyForTermination(instance *autoscaling.Instance) {
	// do we have additional requirements for readiness?
	if a.readinessHandler != nil {
		var (
			hostnames []string
			err       error
		)
		// check if the new nodes all are in ready state
		ids := mapInstancesIds(newInstances)
		hostnames = make([]string, 0)
		for _, i := range ids {
			hostnames = append(hostnames, hostnameMap[i])
		}
		_, err = setScaleDownDisabledAnnotation(hostnames)
		if err != nil {
			log.Warn().Msgf("Unable to set disabled scale down annotations: %v", err)
		}
		unReadyCount, err = readinessHandler.getUnreadyCount(hostnames, ids)
		if err != nil {
			return desired, originalDesired, "", fmt.Errorf("Error getting readiness new node status: %v", err)
		}
		if unReadyCount > 0 {
			return desired, originalDesired, "", fmt.Errorf("[%s] Nodes not ready: %d", *asg.AutoScalingGroupName, unReadyCount)
		}
	}
	candidate := *oldInstances[0].InstanceId

	if readinessHandler != nil {
		// get the node reference - first need the hostname
		var (
			hostname string
			err      error
		)
		hostname = hostnameMap[candidate]
		err = readinessHandler.prepareTermination([]string{hostname}, []string{candidate})
		if err != nil {
			return desired, originalDesired, "", fmt.Errorf("Unexpected error readiness handler terminating node %s: %v", hostname, err)
		}
	}
}
