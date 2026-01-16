package infraprovider

import (
	"encoding/json"
	"fmt"
	"os/exec"

	utilnet "k8s.io/utils/net"
)

const (
	testMachineName = "ovn-kubernetes-e2e"
)

type Net struct {
	Device string `json:"device"`
	Mac    string `json:"mac"`
	Net    string `json:"net"`
	Type   string `json:"type"`
}

type Disk struct {
	Device string `json:"device"`
	Size   int    `json:"size"`
	Format string `json:"format"`
	Type   string `json:"type"`
	Path   string `json:"path"`
}

type Machine struct {
	Name         string   `json:"name"`
	Nets         []Net    `json:"nets"`
	Disks        []Disk   `json:"disks"`
	ID           string   `json:"id"`
	User         string   `json:"user"`
	Image        string   `json:"image"`
	Plan         string   `json:"plan"`
	Profile      string   `json:"profile"`
	CreationDate string   `json:"creationdate"`
	IP           string   `json:"ip"`
	IPs          []string `json:"ips"`
	Status       string   `json:"status"`
	Autostart    bool     `json:"autostart"`
	NumCPUs      int      `json:"numcpus"`
	Memory       int      `json:"memory"`
}

func ensureTestMachine(machineName string) (*machine, error) {
	// Check if machine already exists
	checkCmd := exec.Command("kcli", "show", "vm", machineName)
	if err := checkCmd.Run(); err == nil {
		// Machine exists, retrieve its information
		testMachine, err := showMachine(machineName)
		if err != nil {
			return nil, fmt.Errorf("failed to show existing libvirt virtual machine: %v", err)
		}
		m := &machine{
			name: testMachine.Name,
			ipv4: testMachine.IP,
		}
		// Validate and assign IPv6 if available
		if len(testMachine.IPs) > 1 && utilnet.IsIPv6String(testMachine.IPs[1]) {
			m.ipv6 = testMachine.IPs[1]
		}
		return m, nil
	}

	// Machine doesn't exist, create it
	createCmd := exec.Command("kcli", "create", "vm", "-i", "fedora42", machineName, "--wait",
		"-P", "cmds=['dnf install -y docker','systemctl enable --now docker']")
	output, err := createCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to create libvirt machine: %v, output: %s", err, string(output))
	}
	testMachine, err := showMachine(machineName)
	if err != nil {
		return nil, fmt.Errorf("failed to show libvirt virtual machine after creation: %v", err)
	}
	m := &machine{
		name: testMachine.Name,
		ipv4: testMachine.IP,
	}
	// Validate and assign IPv6 if available
	if len(testMachine.IPs) > 1 && utilnet.IsIPv6String(testMachine.IPs[1]) {
		m.ipv6 = testMachine.IPs[1]
	}
	return m, nil
}

func showMachine(machineName string) (*Machine, error) {
	cmd := exec.Command("kcli", "show", "vm", machineName, "-o", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to show libvirt virtual machine: %v, output: %s", err, string(output))
	}
	vm := &Machine{}
	if err := json.Unmarshal(output, vm); err != nil {
		return nil, fmt.Errorf("failed to unmarshal libvirt virtual machine output: %v, output: %s", err, string(output))
	}
	return vm, nil
}

func removeMachine(machineName string) error {
	cmd := exec.Command("kcli", "remove", "-y", "vm", machineName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete libvirt machine: %s, output: %s, err: %w", machineName, string(output), err)
	}
	return nil
}

func (m *Machine) attachNetwork(networkName string) error {
	cmd := exec.Command("kcli", "add", "nic", m.Name, networkName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"failed to attach network %s to machine %s, output: %s, err: %w",
			networkName,
			m.Name,
			string(output),
			err)
	}
	return nil
}
