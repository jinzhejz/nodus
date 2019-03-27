package exec

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"

	"github.com/IntelAI/nodus/pkg/config"
	"github.com/IntelAI/nodus/pkg/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
)

type ScenarioRunner interface {
	RunScenario(scenario *config.Scenario) error
	RunAssert(step *config.Step) error
	RunCreate(step *config.Step) error
	RunChange(step *config.Step) error
	RunDelete(step *config.Step) error
}

func NewScenarioRunner(client *kubernetes.Clientset, namespace string, nodeConfig *config.NodeConfig, podConfig *config.PodConfig) ScenarioRunner {
	return &runner{
		client:     client,
		namespace:  namespace,
		nodeConfig: nodeConfig,
		podConfig:  podConfig,
	}
}

type runner struct {
	client     *kubernetes.Clientset
	namespace  string
	podConfig  *config.PodConfig
	nodeConfig *config.NodeConfig
}

func (r *runner) RunScenario(scenario *config.Scenario) error {
	log.WithFields(log.Fields{"name": scenario.Name}).Info("run scenario")
	numSteps := len(scenario.Steps)
	for i, step := range scenario.Steps {
		raw := scenario.RawSteps[i]
		log.WithFields(log.Fields{
			"description": raw,
		}).Infof("run step [%d / %d]", i+1, numSteps)

		var err error
		switch step.Verb {
		case config.Assert:
			err = r.RunAssert(step)
		case config.Create:
			err = r.RunCreate(step)
		case config.Change:
			err = r.RunChange(step)
		case config.Delete:
			err = r.RunDelete(step)
		default:
			err = fmt.Errorf("unknown verb `%s`", step.Verb)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) assertNode(assert *config.AssertStep) error {
	// Supported grammar: "assert" <count> [<class>] <object> [<is> <phase>] [<within> <count> seconds]

	// Get all the nodes with the optional class.

	var labelSelector string
	if assert.Class != "" {
		labelSelector = fmt.Sprintf("np.class=%s", assert.Class)
	}
	nodeList, err := r.client.CoreV1().Nodes().List(metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return err
	}
	if nodeList.Items == nil || uint64(len(nodeList.Items)) != assert.Count {
		if assert.Class != "" {
			return fmt.Errorf("found %d nodes of class %s, but %d expected", len(nodeList.Items), assert.Class, assert.Count)
		} else {
			return fmt.Errorf("found %d nodes but %d expected", len(nodeList.Items), assert.Count)
		}
	}
	return nil
}

func (r *runner) assertPod(assert *config.AssertStep) error {
	// Supported grammar: "assert" <count> [<class>] <object> [<is> <phase>] [<is> <phase>] [<within> <count> seconds]
	var labelSelector string
	if assert.Class != "" {
		labelSelector = fmt.Sprintf("np.class=%s", assert.Class)
	}
	var fieldSelector string
	if assert.PodPhase != "" {
		fieldSelector = fmt.Sprintf("status.phase=%s", assert.PodPhase)
	}

	podList, err := r.client.CoreV1().Pods(r.namespace).List(metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return err
	}
	if podList.Items == nil || uint64(len(podList.Items)) != assert.Count {
		return fmt.Errorf("found %d pods of class %s and phase: %s, but %d expected", len(podList.Items), assert.Class, assert.PodPhase, assert.Count)
	}

	return nil
}

func (r *runner) RunAssert(step *config.Step) error {
	if step.Assert == nil {
		return fmt.Errorf("there is no assert in this step.")
	}

	backoffWait := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1,
		Steps:    int(step.Assert.Delay.Seconds()),
	}
	var err error

	switch step.Assert.Object {
	case config.Node:
		for backoffWait.Steps > 0 {
			err = r.assertNode(step.Assert)
			if err == nil {
				break
			}
			time.Sleep(backoffWait.Step())
		}
		return err
	case config.Pod:
		for backoffWait.Steps > 0 {
			err = r.assertPod(step.Assert)
			if err == nil {
				break
			}
			time.Sleep(backoffWait.Step())
		}
		return err
	}
	return fmt.Errorf("assert object: %s not supported", step.Assert.Object)
}

func (r *runner) createNode(create *config.CreateStep) error {
	// Supported grammar: "create" <count> <class> <object>
	// Check if nodeConfig has the specified class
	for _, class := range r.nodeConfig.NodeClasses {
		if config.Class(class.Name) == create.Class {
			for i := uint64(0); i < create.Count; i++ {
				n := node.NewFakeNode(fmt.Sprintf("%s-%d", class.Name, i), class.Name, class.Labels, class.Resources)
				err := n.Start(r.client)
				if err != nil {
					return fmt.Errorf("could not create node of class: %s, err: %s", create.Class, err.Error())
				}
			}
			return nil
		}
	}
	return fmt.Errorf("class: %s not found in the node config", create.Class)
}

func (r *runner) createPod(create *config.CreateStep) error {
	// Supported grammar: "create" <count> <class> <object>

	podClient := r.client.CoreV1().Pods(r.namespace)
	// Check if podConfig has the specified class
	for _, class := range r.podConfig.PodClasses {
		if config.Class(class.Name) == create.Class {
			for i := uint64(0); i < create.Count; i++ {
				// Create the pod
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:   fmt.Sprintf("%s-%d", class.Name, i),
						Labels: class.Labels,
					},
					Spec: class.Spec,
				}
				if _, err := podClient.Create(pod); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return fmt.Errorf("class: %s not found in the pod config", create.Class)
}

func (r *runner) RunCreate(step *config.Step) error {
	if step.Create == nil {
		return fmt.Errorf("there is no create in this step.")
	}

	switch step.Create.Object {
	case config.Node:
		return r.createNode(step.Create)
	case config.Pod:
		return r.createPod(step.Create)
	}

	return fmt.Errorf("create object: %s not supported", step.Create.Object)
}

func (r *runner) changePod(change *config.ChangeStep) error {
	// Supported grammar: "change" <count> <class> <object> "from" <phase> "to" <phase>

	if change.FromPodPhase == change.ToPodPhase {
		return fmt.Errorf("the change requested is to the same phase. From phase: %s, to phase: %s", change.FromPodPhase, change.ToPodPhase)
	}

	pods, err := r.client.CoreV1().Pods(r.namespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("np.class=%s", change.Class),
		FieldSelector: fmt.Sprintf("status.phase=%s", change.FromPodPhase),
	})
	if err != nil {
		return err
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("found 0 pods of class: %s and phase: %s, expected: %d", change.Class, change.FromPodPhase, change.Count)
	}

	if uint64(len(pods.Items)) < change.Count {
		return fmt.Errorf("expected atleast %d pods of class: %s and phase: %s, but found: %d", change.Count, change.Class, change.FromPodPhase, len(pods.Items))
	}

	// Get a slice

	podClient := r.client.CoreV1().Pods(r.namespace)
	for i := uint64(0); i < change.Count; i++ {
		pod := pods.Items[i]
		// Copy pod
		var copy corev1.Pod
		copy = pod
		copy.Status.Phase = change.ToPodPhase
		// Get current conditions
		var cond corev1.ConditionStatus
		if pod.Status.Phase == corev1.PodPending && change.ToPodPhase == corev1.PodRunning {
			cond = corev1.ConditionTrue
		}

		if change.ToPodPhase == corev1.PodSucceeded || change.ToPodPhase == corev1.PodFailed {
			cond = corev1.ConditionFalse
		}

		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:               corev1.PodConditionType(change.ToPodPhase),
			Status:             cond,
			LastTransitionTime: metav1.Now(),
		})
		_, err := podClient.UpdateStatus(&copy)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) RunChange(step *config.Step) error {
	if step.Change == nil {
		return fmt.Errorf("there is no change in this step.")
	}
	switch step.Change.Object {
	case config.Pod:
		return r.changePod(step.Change)
	}
	return fmt.Errorf("change object: %s not supported", step.Change.Object)
}

func (r *runner) deleteNode(delete *config.DeleteStep) error {
	// Supported grammar: "delete" <count> <class> <object>
	nodes, err := r.client.CoreV1().Nodes().List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("np.class=%s", delete.Class),
	})
	if err != nil {
		return fmt.Errorf("no nodes found for class: %s", delete.Class)
	}
	if uint64(len(nodes.Items)) < delete.Count {
		return fmt.Errorf("found %d nodes of class: %s, but expected: %d", len(nodes.Items), delete.Class, delete.Count)
	}

	for i := uint64(0); i < delete.Count; i++ {
		err = r.client.CoreV1().Nodes().Delete(nodes.Items[i].Name, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *runner) deletePod(delete *config.DeleteStep) error {
	// Supported grammar: "delete" <count> <class> <object>
	pods, err := r.client.CoreV1().Pods(r.namespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("np.class=%s", delete.Class),
	})
	if err != nil {
		return fmt.Errorf("no pods found for class: %s", delete.Class)
	}
	if uint64(len(pods.Items)) < delete.Count {
		return fmt.Errorf("found %d pods of class: %s, but expected: %d", len(pods.Items), delete.Class, delete.Count)
	}

	for i := uint64(0); i < delete.Count; i++ {
		err = r.client.CoreV1().Pods(r.namespace).Delete(pods.Items[i].Name, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) RunDelete(step *config.Step) error {
	if step.Delete == nil {
		return fmt.Errorf("there is no delete in this step.")
	}

	switch step.Delete.Object {
	case config.Node:
		return r.deleteNode(step.Delete)
	case config.Pod:
		return r.deletePod(step.Delete)
	}

	return fmt.Errorf("delete object: %s not supported", step.Delete.Object)
}
