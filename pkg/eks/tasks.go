package eks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"

	"github.com/weaveworks/eksctl/pkg/actions/identityproviders"
	"github.com/weaveworks/eksctl/pkg/windows"

	"github.com/kris-nova/logger"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/weaveworks/eksctl/pkg/actions/irsa"
	"github.com/weaveworks/eksctl/pkg/addons"
	"github.com/weaveworks/eksctl/pkg/cfn/manager"
	"github.com/weaveworks/eksctl/pkg/fargate"
	iamoidc "github.com/weaveworks/eksctl/pkg/iam/oidc"
	instanceutils "github.com/weaveworks/eksctl/pkg/utils/instance"
	"github.com/weaveworks/eksctl/pkg/utils/tasks"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/kubernetes"
)

type clusterConfigTask struct {
	info string
	spec *api.ClusterConfig
	call func(*api.ClusterConfig) error
}

func (t *clusterConfigTask) Describe() string { return t.info }

func (t *clusterConfigTask) Do(errs chan error) error {
	err := t.call(t.spec)
	close(errs)
	return err
}

// WindowsIPAMTask is a task for enabling Windows IPAM.
type WindowsIPAMTask struct {
	Info          string
	ClientsetFunc func() (kubernetes.Interface, error)
}

// Do implements Task.
func (w *WindowsIPAMTask) Do(errCh chan error) error {
	defer close(errCh)

	clientset, err := w.ClientsetFunc()
	if err != nil {
		return err
	}
	windowsIPAM := windows.IPAM{
		Clientset: clientset,
	}
	return windowsIPAM.Enable(context.TODO())
}

// Describe implements Task.
func (w *WindowsIPAMTask) Describe() string {
	return w.Info
}

// VPCControllerTask represents a task to install the VPC controller
type VPCControllerTask struct {
	Info            string
	ClusterProvider *ClusterProvider
	ClusterConfig   *api.ClusterConfig
	PlanMode        bool
}

// Describe implements Task
func (v *VPCControllerTask) Describe() string { return v.Info }

// Do implements Task
func (v *VPCControllerTask) Do(errCh chan error) error {
	defer close(errCh)
	rawClient, err := v.ClusterProvider.NewRawClient(v.ClusterConfig)
	if err != nil {
		return err
	}
	oidc, err := v.ClusterProvider.NewOpenIDConnectManager(v.ClusterConfig)
	if err != nil {
		return err
	}

	stackCollection := manager.NewStackCollection(v.ClusterProvider.Provider, v.ClusterConfig)

	clientSet, err := v.ClusterProvider.NewStdClientSet(v.ClusterConfig)
	if err != nil {
		return err
	}
	irsaManager := irsa.New(v.ClusterConfig.Metadata.Name, stackCollection, oidc, clientSet)
	irsa := addons.NewIRSAHelper(oidc, stackCollection, irsaManager, v.ClusterConfig.Metadata.Name)

	// TODO PlanMode doesn't work as intended
	vpcController := addons.NewVPCController(rawClient, irsa, v.ClusterConfig.Status, v.ClusterProvider.Provider.Region(), v.PlanMode)
	if err := vpcController.Deploy(); err != nil {
		return errors.Wrap(err, "error installing VPC controller")
	}
	return nil
}

type devicePluginTask struct {
	kind            string
	clusterProvider *ClusterProvider
	spec            *api.ClusterConfig
	mkPlugin        addons.MkDevicePlugin
	logMessage      string
}

func (n *devicePluginTask) Describe() string { return fmt.Sprintf("install %s device plugin", n.kind) }

func (n *devicePluginTask) Do(errCh chan error) error {
	defer close(errCh)
	rawClient, err := n.clusterProvider.NewRawClient(n.spec)
	if err != nil {
		return err
	}
	devicePlugin := n.mkPlugin(rawClient, n.clusterProvider.Provider.Region(), false)
	if err := devicePlugin.Deploy(); err != nil {
		return errors.Wrap(err, "error installing device plugin")
	}
	logger.Info(n.logMessage)
	return nil
}

func newNvidiaDevicePluginTask(
	clusterProvider *ClusterProvider,
	spec *api.ClusterConfig,
) tasks.Task {
	t := devicePluginTask{
		kind:            "Nvidia",
		clusterProvider: clusterProvider,
		spec:            spec,
		mkPlugin:        addons.NewNvidiaDevicePlugin,
		logMessage: `as you are using the EKS-Optimized Accelerated AMI with a GPU-enabled instance type, the Nvidia Kubernetes device plugin was automatically installed.
	to skip installing it, use --install-nvidia-plugin=false.
`,
	}
	return &t
}

func newNeuronDevicePluginTask(
	clusterProvider *ClusterProvider,
	spec *api.ClusterConfig,
) tasks.Task {
	t := devicePluginTask{
		kind:            "Neuron",
		clusterProvider: clusterProvider,
		spec:            spec,
		mkPlugin:        addons.NewNeuronDevicePlugin,
		logMessage: `as you are using the EKS-Optimized Accelerated AMI with an inf1 instance type, the AWS Neuron Kubernetes device plugin was automatically installed.
	to skip installing it, use --install-neuron-plugin=false.
`,
	}
	return &t
}

func newEFADevicePluginTask(
	clusterProvider *ClusterProvider,
	spec *api.ClusterConfig,
) tasks.Task {
	t := devicePluginTask{
		kind:            "EFA",
		clusterProvider: clusterProvider,
		spec:            spec,
		mkPlugin:        addons.NewEFADevicePlugin,
		logMessage:      "as you have enabled EFA, the EFA device plugin was automatically installed.",
	}
	return &t
}

type restartDaemonsetTask struct {
	name            string
	namespace       string
	clusterProvider *ClusterProvider
	spec            *api.ClusterConfig
}

func (t *restartDaemonsetTask) Describe() string {
	return fmt.Sprintf(`restart daemonset "%s/%s"`, t.namespace, t.name)
}

func (t *restartDaemonsetTask) Do(errCh chan error) error {
	defer close(errCh)
	clientSet, err := t.clusterProvider.NewStdClientSet(t.spec)
	if err != nil {
		return err
	}
	ds := clientSet.AppsV1().DaemonSets(t.namespace)
	dep, err := ds.Get(context.TODO(), t.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = make(map[string]string)
	}
	dep.Spec.Template.Annotations["eksctl.io/restartedAt"] = time.Now().Format(time.RFC3339)
	bytes, err := runtime.Encode(unstructured.UnstructuredJSONScheme, dep)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal %q deployment", t.name)
	}
	if _, err := ds.Patch(context.TODO(), t.name, types.MergePatchType, bytes, metav1.PatchOptions{}); err != nil {
		return errors.Wrap(err, "failed to patch deployment")
	}
	logger.Info(`daemonset "%s/%s" restarted`, t.namespace, t.name)
	return nil
}

// CreateExtraClusterConfigTasks returns all tasks for updating cluster configuration not depending on the control plane availability
func (c *ClusterProvider) CreateExtraClusterConfigTasks(cfg *api.ClusterConfig) *tasks.TaskTree {
	newTasks := &tasks.TaskTree{
		Parallel:  false,
		IsSubTask: true,
	}

	newTasks.Append(&tasks.GenericTask{
		Description: "wait for control plane to become ready",
		Doer: func() error {
			clientSet, err := c.NewStdClientSet(cfg)
			if err != nil {
				return errors.Wrap(err, "error creating Clientset")
			}
			if err := c.WaitForControlPlane(cfg.Metadata, clientSet); err != nil {
				return err
			}
			return c.RefreshClusterStatus(cfg)
		},
	})

	if cfg.HasClusterCloudWatchLogging() {
		if logRetentionDays := cfg.CloudWatch.ClusterLogging.LogRetentionInDays; logRetentionDays != 0 {
			newTasks.Append(&clusterConfigTask{
				info: "update CloudWatch log retention",
				spec: cfg,
				call: func(clusterConfig *api.ClusterConfig) error {
					_, err := c.Provider.CloudWatchLogs().PutRetentionPolicy(&cloudwatchlogs.PutRetentionPolicyInput{
						// The format for log group name is documented here: https://docs.aws.amazon.com/eks/latest/userguide/control-plane-logs.html
						LogGroupName:    aws.String(fmt.Sprintf("/aws/eks/%s/cluster", cfg.Metadata.Name)),
						RetentionInDays: aws.Int64(int64(logRetentionDays)),
					})
					if err != nil {
						return errors.Wrap(err, "error updating log retention settings")
					}
					logger.Info("set log retention to %d days for CloudWatch logging", logRetentionDays)
					return nil
				},
			})
		}
	}

	if cfg.IsFargateEnabled() {
		manager := fargate.NewFromProvider(cfg.Metadata.Name, c.Provider, c.NewStackManager(cfg))
		newTasks.Append(&fargateProfilesTask{
			info:            "create fargate profiles",
			spec:            cfg,
			clusterProvider: c,
			manager:         &manager,
		})
	}

	if api.IsEnabled(cfg.IAM.WithOIDC) {
		c.appendCreateTasksForIAMServiceAccounts(cfg, newTasks)
	}

	if len(cfg.IdentityProviders) > 0 {
		newTasks.Append(identityproviders.NewAssociateProvidersTask(*cfg.Metadata, cfg.IdentityProviders, c.Provider.EKS()))
	}

	if cfg.HasWindowsNodeGroup() {
		newTasks.Append(&WindowsIPAMTask{
			Info: "enable Windows IP address management",
			ClientsetFunc: func() (kubernetes.Interface, error) {
				return c.NewStdClientSet(cfg)
			},
		})
	}

	return newTasks
}

// LogEnabledFeatures logs enabled features
func LogEnabledFeatures(clusterConfig *api.ClusterConfig) {
	if clusterConfig.HasClusterEndpointAccess() && api.EndpointsEqual(*clusterConfig.VPC.ClusterEndpoints, *api.ClusterEndpointAccessDefaults()) {
		logger.Info(clusterConfig.DefaultEndpointsMsg())
	} else {
		logger.Info(clusterConfig.CustomEndpointsMsg())
	}

	if !clusterConfig.HasClusterCloudWatchLogging() {
		logger.Info("CloudWatch logging will not be enabled for cluster %q in %q", clusterConfig.Metadata.Name, clusterConfig.Metadata.Region)
		logger.Info("you can enable it with 'eksctl utils update-cluster-logging --enable-types={SPECIFY-YOUR-LOG-TYPES-HERE (e.g. all)} --region=%s --cluster=%s'", clusterConfig.Metadata.Region, clusterConfig.Metadata.Name)
		return
	}

	all := sets.NewString(api.SupportedCloudWatchClusterLogTypes()...)

	enabled := sets.NewString()
	if clusterConfig.HasClusterCloudWatchLogging() {
		enabled.Insert(clusterConfig.CloudWatch.ClusterLogging.EnableTypes...)
	}

	disabled := all.Difference(enabled)

	describeEnabledTypes := "no types enabled"
	if enabled.Len() > 0 {
		describeEnabledTypes = fmt.Sprintf("enabled types: %s", strings.Join(enabled.List(), ", "))
	}

	describeDisabledTypes := "no types disabled"
	if disabled.Len() > 0 {
		describeDisabledTypes = fmt.Sprintf("disabled types: %s", strings.Join(disabled.List(), ", "))
	}

	logger.Info("configuring CloudWatch logging for cluster %q in %q (%s & %s)",
		clusterConfig.Metadata.Name, clusterConfig.Metadata.Region, describeEnabledTypes, describeDisabledTypes,
	)
}

// ClusterTasksForNodeGroups returns all tasks dependent on node groups
func (c *ClusterProvider) ClusterTasksForNodeGroups(cfg *api.ClusterConfig, installNeuronDevicePluginParam, installNvidiaDevicePluginParam bool) *tasks.TaskTree {
	tasks := &tasks.TaskTree{
		Parallel:  true,
		IsSubTask: false,
	}
	var needsNvidiaButNotNeuron = func(t string) bool {
		return instanceutils.IsGPUInstanceType(t) && !instanceutils.IsInferentiaInstanceType(t)
	}
	var haveNeuronInstanceType, haveNvidiaInstanceType, efaEnabled bool
	for _, ng := range cfg.NodeGroups {
		haveNeuronInstanceType = haveNeuronInstanceType || api.HasInstanceType(ng, instanceutils.IsInferentiaInstanceType)
		haveNvidiaInstanceType = haveNvidiaInstanceType || api.HasInstanceType(ng, needsNvidiaButNotNeuron)
		efaEnabled = efaEnabled || api.IsEnabled(ng.EFAEnabled)
	}
	for _, ng := range cfg.ManagedNodeGroups {
		haveNeuronInstanceType = haveNeuronInstanceType || api.HasInstanceTypeManaged(ng, instanceutils.IsInferentiaInstanceType)
		haveNvidiaInstanceType = haveNvidiaInstanceType || api.HasInstanceTypeManaged(ng, needsNvidiaButNotNeuron)
		efaEnabled = efaEnabled || api.IsEnabled(ng.EFAEnabled)
	}
	if haveNeuronInstanceType {
		if installNeuronDevicePluginParam {
			tasks.Append(newNeuronDevicePluginTask(c, cfg))
		} else {
			logger.Info("as you are using the EKS-Optimized Accelerated AMI with an inf1 instance type, you will need to install the AWS Neuron Kubernetes device plugin.")
			logger.Info("\t see the following page for instructions: https://awsdocs-neuron.readthedocs-hosted.com/en/latest/neuron-deploy/tutorials/tutorial-k8s.html#tutorial-k8s-env-setup-for-neuron")
		}
	}
	if haveNvidiaInstanceType {
		if installNvidiaDevicePluginParam {
			tasks.Append(newNvidiaDevicePluginTask(c, cfg))
		} else {
			logger.Info("as you are using a GPU optimized instance type you will need to install NVIDIA Kubernetes device plugin.")
			logger.Info("\t see the following page for instructions: https://github.com/NVIDIA/k8s-device-plugin")
		}
	}

	var ngs []*api.NodeGroupBase
	for _, ng := range cfg.NodeGroups {
		ngs = append(ngs, ng.NodeGroupBase)
	}
	for _, ng := range cfg.ManagedNodeGroups {
		ngs = append(ngs, ng.NodeGroupBase)
	}
	for _, ng := range ngs {
		if len(ng.ASGSuspendProcesses) > 0 {
			tasks.Append(newSuspendProcesses(c, cfg, ng))
		}
	}

	if efaEnabled {
		tasks.Append(newEFADevicePluginTask(c, cfg))
	}

	return tasks
}

func (c *ClusterProvider) appendCreateTasksForIAMServiceAccounts(cfg *api.ClusterConfig, tasks *tasks.TaskTree) {
	// we don't have all the information to construct full iamoidc.OpenIDConnectManager now,
	// instead we just create a reference that gets updated when first task runs, and gets
	// used by this would be more elegant if it was all done via CloudFormation and we didn't
	// have to put wires across all the things like this; this whole function is needed because
	// we cannot manage certain EKS features with CloudFormation
	oidcPlaceholder := &iamoidc.OpenIDConnectManager{}
	tasks.Append(&clusterConfigTask{
		info: "associate IAM OIDC provider",
		spec: cfg,
		call: func(cfg *api.ClusterConfig) error {
			oidc, err := c.NewOpenIDConnectManager(cfg)
			if err != nil {
				return err
			}
			if err := oidc.CreateProvider(); err != nil {
				return err
			}
			*oidcPlaceholder = *oidc
			// Make sure control plane is reachable
			clientSet, err := c.NewStdClientSet(cfg)
			if err != nil {
				return errors.Wrap(err, "failed to get ClientSet")
			}
			if err := c.WaitForControlPlane(cfg.Metadata, clientSet); err != nil {
				return errors.Wrap(err, "failed to wait for control plane")
			}
			return nil
		},
	})

	clientSet := &kubernetes.CallbackClientSet{
		Callback: func() (kubernetes.Interface, error) {
			return c.NewStdClientSet(cfg)
		},
	}

	// as this is non-CloudFormation context, we need to construct a new stackManager,
	// given a clientSet getter and OpenIDConnectManager reference we can build out
	// the list of tasks for each of the service accounts that need to be created
	newTasks := c.NewStackManager(cfg).NewTasksToCreateIAMServiceAccounts(
		api.IAMServiceAccountsWithImplicitServiceAccounts(cfg),
		oidcPlaceholder,
		clientSet,
	)
	newTasks.IsSubTask = true
	tasks.Append(newTasks)
	tasks.Append(&restartDaemonsetTask{
		namespace:       "kube-system",
		name:            "aws-node",
		clusterProvider: c,
		spec:            cfg,
	})
}
