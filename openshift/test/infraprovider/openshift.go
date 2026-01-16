package infraprovider

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	machineclient "github.com/openshift/client-go/machine/clientset/versioned"

	ovnkconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/containerengine"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/portalloc"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	machineUserName      = "fedora"
	machineCreationLimit = 5
)

// machine maps 1-1 with OpenShift machine and therefore represents either a VM or BM host
type machine struct {
	name string
	ipv4 string
	ipv6 string
	// container that is hosted by this machine
	container api.ExternalContainer
	active    bool
}

func (m *machine) hasIPv4Addr() bool {
	return m.ipv4 != ""
}

func (m *machine) hasIPv6Addr() bool {
	return m.ipv6 != ""
}

func (m *machine) getValidIP() (string, error) {
	if m.hasIPv4Addr() {
		return m.ipv4, nil
	}
	if m.hasIPv6Addr() {
		return m.ipv6, nil
	}
	return "", fmt.Errorf("machine has no valid IP address set")
}

func (m *machine) isHostingContainer() bool {
	return m.active
}

func (m *machine) addContainer(container api.ExternalContainer) error {
	if m.isHostingContainer() {
		return fmt.Errorf("unable to add container to a machine which already hosts a container")
	}
	testMachine, err := showMachine(m.name)
	if err != nil {
		return fmt.Errorf("failed retrieving test machine: %w", err)
	}
	if err := testMachine.attachNetwork(container.Network.Name()); err != nil {
		return fmt.Errorf("failed attaching network %q to machine %q", container.Network.Name(), testMachine.Name)
	}
	cmd := buildDaemonContainerCmd(container.Name, container.Image, container.CmdArgs)
	cmd = addElevatedPrivileges(cmd)
	if _, err := m.execCmd(cmd); err != nil {
		return fmt.Errorf("failed to execute command on machine %s: %w", m.name, err)
	}
	m.container = container
	m.active = true
	return nil
}

func (m *machine) deleteContainer(container api.ExternalContainer) error {
	if !m.isHostingContainer() {
		return fmt.Errorf("attempted to delete a container when the machine doesn't host one")
	}
	isRunning, err := m.isContainerRunning(container.Name)
	if err != nil {
		return fmt.Errorf("failed to check if container is running: %v", err)
	}
	if !isRunning {
		m.active = false
		return nil
	}
	// remove the container
	cmd := buildRemoveContainerCmd(container.Name)
	cmd = addElevatedPrivileges(cmd)
	if _, err := m.execCmd(cmd); err != nil {
		return fmt.Errorf("failed to execute command on machine %s: %w", m.name, err)
	}
	m.active = false
	return nil
}

func (m *machine) getContainerLogs(container api.ExternalContainer) (string, error) {
	isRunning, err := m.isContainerRunning(container.Name)
	if err != nil {
		return "", fmt.Errorf("failed to check if container is running: %v", err)
	}
	if !isRunning {
		return "", fmt.Errorf("external container is not running on machine")
	}
	logsCmd := buildContainerLogsCmd(container.Name)
	logsCmd = addElevatedPrivileges(logsCmd)
	res, err := m.execCmd(logsCmd)
	if err != nil {
		return "", fmt.Errorf("failed to execute command (%s) within machine %s: err: %v", logsCmd, m.name, err)
	}
	return res.stdout, nil
}

func (m *machine) execCmd(cmd string) (result, error) {
	ginkgo.By("Running command on test machine: " + cmd)
	var r result
	signer, err := getSigner()
	if err != nil {
		return r, fmt.Errorf("error getting signer: %w", err)
	}
	ip, err := m.getValidIP()
	if err != nil {
		return r, fmt.Errorf("failed to get valid IP: %w", err)
	}
	r, err = runSSHCommand(cmd, ip+":22", signer)
	if err != nil {
		return r, fmt.Errorf("failed to run SSH command for %s@%s: %w: %+v", machineUserName, ip, err, r)
	}
	return r, nil

}

func (m *machine) execContainerCmd(container api.ExternalContainer, cmd []string) (result, error) {
	containerCmd := buildContainerCmd(container.Name, cmd)
	containerCmd = addElevatedPrivileges(containerCmd)
	return m.execCmd(containerCmd)
}

func (m *machine) isContainerRunning(name string) (bool, error) {
	// check to see if the container is running before attempting to delete it
	isPresentCmd := buildContainerCheckCmd(name)
	isPresentCmd = addElevatedPrivileges(isPresentCmd)
	r, err := m.execCmd(isPresentCmd)
	if err != nil {
		return false, fmt.Errorf("failed to execute command on machine %s: %s", m.name, r)
	}
	if r.getStdOut() != "" {
		return true, nil
	}
	return false, nil
}

type machines struct {
	mu   *sync.Mutex
	list []*machine
}

type openshift struct {
	externalContainerPortAlloc *portalloc.PortAllocator
	hostPortAlloc              *portalloc.PortAllocator
	kubeClient                 *kubernetes.Clientset
	machineClient              *machineclient.Clientset
	sharedExternalMachines     *machines
}

func (o openshift) ShutdownNode(nodeName string) error {
	return fmt.Errorf("ShutdownNode not implemented for OpenShift provider")
}

func (o openshift) StartNode(nodeName string) error {
	return fmt.Errorf("StartNode not implemented for OpenShift provider")
}

func (m openshift) GetDefaultTimeoutContext() *framework.TimeoutContext {
	timeouts := framework.NewTimeoutContext()
	timeouts.PodStart = 10 * time.Minute
	return timeouts
}

func IsProvider(config *rest.Config) (bool, error) {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	// Check for OpenShift-specific API groups
	groups, err := kubeClient.Discovery().ServerGroups()
	if err != nil {
		return false, fmt.Errorf("failed to get server groups: %w", err)
	}
	for _, group := range groups.Groups {
		if strings.HasSuffix(group.Name, ".openshift.io") {
			return true, nil
		}
	}
	return false, nil
}

func New(config *rest.Config) (api.Provider, error) {
	ovnkconfig.Kubernetes.DNSServiceNamespace = "openshift-dns"
	ovnkconfig.Kubernetes.DNSServiceName = "dns-default"
	machineClient, err := machineclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create machine client: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create kubernetes client: %w", err)
	}
	o := openshift{externalContainerPortAlloc: portalloc.New(30000, 32767), hostPortAlloc: portalloc.New(30000, 32767),
		machineClient: machineClient, kubeClient: kubeClient, sharedExternalMachines: &machines{mu: &sync.Mutex{}, list: make([]*machine, 0)}}
	ginkgo.AfterSuite(o.cleanUp)
	return o, nil
}

func (o openshift) Name() string {
	return "openshift"
}

func (o openshift) PrimaryNetwork() (api.Network, error) {
	return &openshiftNetwork{
		name: "default",
	}, nil
}

func (o openshift) ExternalContainerPrimaryInterfaceName() string {
	return "eth0"
}

func (o openshift) GetNetwork(name string) (api.Network, error) {
	panic("not implemented")
}

func (o openshift) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	panic("not implemented")
}

func (o openshift) GetK8NodeNetworkInterface(instance string, network api.Network) (api.NetworkInterface, error) {
	panic("not implemented")
}

func (o openshift) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	if !container.IsIPv6() && !container.IsIPv4() {
		return "", fmt.Errorf("expected either IPv4 or IPv6 address to be set")
	}
	return o.execMachine(container, []string{containerengine.Get().String(), "logs", container.Name})
}

func (o openshift) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	if len(cmd) == 0 {
		panic("ExecK8NodeCommand(): insufficient command arguments")
	}
	cmd = append([]string{"debug", fmt.Sprintf("node/%s", nodeName), "--to-namespace=default",
		"--", "chroot", "/host"}, cmd...)
	ocDebugCmd := exec.Command("oc", cmd...)
	var stdout, stderr bytes.Buffer
	ocDebugCmd.Stdout = &stdout
	ocDebugCmd.Stderr = &stderr

	if err := ocDebugCmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run command %q on node %s: %v, stdout: %s, stderr: %s", ocDebugCmd.String(), nodeName, err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func (o openshift) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	if len(cmd) == 0 {
		panic("ExecExternalContainerCommand(): insufficient command arguments")
	}
	if !container.IsIPv6() && !container.IsIPv4() {
		return "", fmt.Errorf("expected either IPv4 or IPv6 address to be set")
	}
	return o.execContainer(container, cmd)
}

func (o openshift) GetExternalContainerPort() uint16 {
	return o.externalContainerPortAlloc.Allocate()
}

func (o openshift) GetK8HostPort() uint16 {
	return o.hostPortAlloc.Allocate()
}

func (o openshift) NewTestContext() api.Context {
	co := &contextOpenshift{32700, o.kubeClient, o.machineClient, o.sharedExternalMachines,
		make([]api.ExternalContainer, 0), make([]func() error, 0)}
	ginkgo.DeferCleanup(co.CleanUp)
	return co
}

func (o openshift) cleanUp() {
	// Clean up all machines that were created
	o.sharedExternalMachines.mu.Lock()
	defer o.sharedExternalMachines.mu.Unlock()

	for _, m := range o.sharedExternalMachines.list {
		if err := removeMachine(m.name); err != nil {
			ginkgo.GinkgoLogr.Error(err, "Failed to remove test machine during cleanup", "machine", m.name)
		}
	}
}

func (o *openshift) execMachine(container api.ExternalContainer, cmd []string) (string, error) {
	o.sharedExternalMachines.mu.Lock()
	defer o.sharedExternalMachines.mu.Unlock()
	m, err := getMachineForExternalContainer(container, o.sharedExternalMachines.list)
	if err != nil {
		return "", err
	}
	r, err := m.execCmd(strings.Join(cmd, " "))
	if err != nil {
		return "", fmt.Errorf("failed to execute command on remote machine: %v - result: %q", err, r)
	}
	return r.getStdOut(), nil
}

func (o *openshift) execContainer(container api.ExternalContainer, cmd []string) (string, error) {
	o.sharedExternalMachines.mu.Lock()
	defer o.sharedExternalMachines.mu.Unlock()
	m, err := getMachineForExternalContainer(container, o.sharedExternalMachines.list)
	if err != nil {
		return "", err
	}
	r, err := m.execContainerCmd(container, cmd)
	if err != nil {
		return "", fmt.Errorf("failed to execute command on remote container within machine: %v - result: %q", err, r)
	}
	return r.getStdOut(), nil
}

type contextOpenshift struct {
	containerPort     int
	kubeClient        *kubernetes.Clientset
	machineClient     *machineclient.Clientset
	sharedMachines    *machines
	cleanUpContainers []api.ExternalContainer
	cleanUpFns        []func() error
}

func (c *contextOpenshift) GetAllowedExternalContainerPort() int {
	port := c.containerPort
	c.containerPort += 1
	return port
}

func (c *contextOpenshift) CreateExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	if valid, err := container.IsValidPreCreateContainer(); !valid {
		return container, fmt.Errorf("failed to create external container: %v", err)
	}
	c.sharedMachines.mu.Lock()
	defer c.sharedMachines.mu.Unlock()

	m, err := c.getInActiveMachine()
	if err != nil {
		return container, fmt.Errorf("failed to find an available machine to host the container: %v", err)
	}
	if err = m.addContainer(container); err != nil {
		return container, fmt.Errorf("failed to add container to machine %s: %v", m.name, err)
	}
	container.IPv4 = m.ipv4
	container.IPv6 = m.ipv6
	if valid, err := container.IsValidPostCreate(); !valid {
		return container, fmt.Errorf("failed to validate external container post creation: %v", err)
	}
	c.cleanUpContainers = append(c.cleanUpContainers, container)
	return container, nil
}

func (c *contextOpenshift) DeleteExternalContainer(container api.ExternalContainer) error {
	if valid, err := container.IsValidPreDelete(); !valid {
		return fmt.Errorf("external container is invalid: %v", err)
	}
	c.sharedMachines.mu.Lock()
	defer c.sharedMachines.mu.Unlock()
	m, err := getMachineForExternalContainer(container, c.sharedMachines.list)
	if err != nil {
		return err
	}
	return m.deleteContainer(container)
}

func (c *contextOpenshift) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	c.sharedMachines.mu.Lock()
	defer c.sharedMachines.mu.Unlock()
	m, err := getMachineForExternalContainer(container, c.sharedMachines.list)
	if err != nil {
		return "", err
	}
	return m.getContainerLogs(container)
}

func (c contextOpenshift) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	panic("not implemented")
}

func (c contextOpenshift) DeleteNetwork(network api.Network) error {
	panic("not implemented")
}

func (c *contextOpenshift) GetAttachedNetworks() (api.Networks, error) {
	panic("not implemented")
}

func (c *contextOpenshift) SetupUnderlay(f *framework.Framework, underlay api.Underlay) error {
	panic("not implemented")
}

func (c contextOpenshift) AttachNetwork(network api.Network, instance string) (api.NetworkInterface, error) {
	panic("not implemented")
}

func (c contextOpenshift) DetachNetwork(network api.Network, instance string) error {
	panic("not implemented")
}

func (c *contextOpenshift) AddCleanUpFn(cleanUpFn func() error) {
	c.cleanUpFns = append(c.cleanUpFns, cleanUpFn)
}

func (c *contextOpenshift) CleanUp() error {
	ginkgo.By("Cleaning up openshift test context")
	var errs []error

	// Clean up external containers
	for i := len(c.cleanUpContainers) - 1; i >= 0; i-- {
		if err := c.DeleteExternalContainer(c.cleanUpContainers[i]); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete external container %s: %w", c.cleanUpContainers[i].Name, err))
		}
	}
	c.cleanUpContainers = nil

	// Generic cleanup activities
	for i := len(c.cleanUpFns) - 1; i >= 0; i-- {
		if err := c.cleanUpFns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	c.cleanUpFns = nil
	return condenseErrors(errs)
}

// getInActiveMachine finds a machine that is inactive or creates a new machine
func (c *contextOpenshift) getInActiveMachine() (*machine, error) {

	// check if there's a machine that is free
	for _, m := range c.sharedMachines.list {
		if !m.active {
			return m, nil
		}
	}
	if len(c.sharedMachines.list) >= machineCreationLimit {
		return nil, fmt.Errorf("cannot create more machines because limit (%d) reached", machineCreationLimit)
	}
	newMachine, err := c.addMachine()
	if err != nil {
		return nil, fmt.Errorf("failed to add test machine: %v", err)
	}
	c.sharedMachines.list = append(c.sharedMachines.list, newMachine)
	return newMachine, nil
}

func (c contextOpenshift) addMachine() (*machine, error) {
	// Generate unique machine name using the index
	machineName := fmt.Sprintf("%s-%d", testMachineName, len(c.sharedMachines.list))
	return ensureTestMachine(machineName)
}

func condenseErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	err := errs[0]
	for _, e := range errs[1:] {
		err = errors.Join(err, e)
	}
	return err
}

func getMachineForExternalContainer(container api.ExternalContainer, machines []*machine) (*machine, error) {
	for _, machine := range machines {
		if !machine.active {
			continue
		}
		if machine.ipv4 == container.IPv4 || machine.ipv6 == container.IPv6 {
			return machine, nil
		}
	}
	return nil, fmt.Errorf("failed to find machine which hosts external container: %q", container.String())
}

func addElevatedPrivileges(cmd string) string {
	return fmt.Sprintf("sudo %s", cmd)
}

// escapeShellArgument properly quotes a string for use as a single argument in a shell command.
// This is a simplified version and might not cover all edge cases for all shells.
// For robust shell escaping, consider using a dedicated library if available,
// or ensure your remote shell is predictable (e.g., always bash).
func escapeShellArgument(arg string) string {
	// Simple rule: if it contains spaces or special characters, single-quote it.
	// Within single quotes, single quotes themselves need to be handled: '\''
	if strings.ContainsAny(arg, " \t\n\r\"'\\`$!{}[]()<>*?~#&;|") {
		return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return arg
}

func buildContainerCmd(name string, cmd []string) string {
	var b strings.Builder
	b.WriteString(containerengine.Get().String())
	b.WriteString(" exec -t ")
	b.WriteString(escapeShellArgument(name))
	b.WriteString(" ") // Add space after container name

	for i, arg := range cmd {
		if i > 0 { // Add space before subsequent arguments
			b.WriteString(" ")
		}
		b.WriteString(escapeShellArgument(arg))
	}

	return b.String()
}

func buildDaemonContainerCmd(name, image string, cmd []string) string {
	var b strings.Builder
	b.WriteString(containerengine.Get().String())
	b.WriteString(" run -itd --privileged --name ")
	b.WriteString(escapeShellArgument(name))
	b.WriteString(" --network host --hostname ")
	b.WriteString(escapeShellArgument(name))
	b.WriteString(" ")
	b.WriteString(escapeShellArgument(image))
	for _, arg := range cmd {
		b.WriteString(" ")
		b.WriteString(escapeShellArgument(arg))
	}
	return b.String()
}

func buildContainerCheckCmd(name string) string {
	return fmt.Sprintf("%s ps -f Name=^%s$ -q", containerengine.Get(), escapeShellArgument(name))
}

func buildContainerLogsCmd(name string) string {
	return fmt.Sprintf("%s logs %s", containerengine.Get(), escapeShellArgument(name))
}

func buildRemoveContainerCmd(name string) string {
	return fmt.Sprintf("%s rm -f %s", containerengine.Get(), escapeShellArgument(name))
}
