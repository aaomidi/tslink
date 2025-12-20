package netutil

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/aaomidi/tslink/pkg/logger"
)

// CreateVethPair creates a veth pair with the given base name.
// Returns the host-side name and container-side name.
func CreateVethPair(baseName string) (string, string, error) {
	hostName := "veth" + baseName
	containerName := "veth" + baseName + "c"

	// Delete existing interfaces if they exist
	if link, err := netlink.LinkByName(hostName); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			logger.Warn("failed to delete existing veth %s: %v", hostName, err)
		}
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: hostName,
			MTU:  1500,
		},
		PeerName: containerName,
	}

	if err := netlink.LinkAdd(veth); err != nil {
		return "", "", fmt.Errorf("failed to create veth pair: %w", err)
	}

	link, err := netlink.LinkByName(hostName)
	if err != nil {
		return "", "", fmt.Errorf("failed to get host veth: %w", err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return "", "", fmt.Errorf("failed to bring up host veth: %w", err)
	}

	logger.Debug("Created veth pair: %s <-> %s", hostName, containerName)

	return hostName, containerName, nil
}

// MoveToNetNS moves a network interface to the specified network namespace.
func MoveToNetNS(ifName string, nsPath string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}

	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get netns from %s: %w", nsPath, err)
	}
	defer ns.Close()

	if err := netlink.LinkSetNsFd(link, int(ns)); err != nil {
		return fmt.Errorf("failed to move %s to netns: %w", ifName, err)
	}

	logger.Debug("Moved interface %s to netns %s", ifName, nsPath)

	return nil
}

// SetupInterfaceInNS sets up an interface inside a network namespace.
func SetupInterfaceInNS(nsPath string, ifName string, newName string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get current namespace
	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	// Switch to target namespace
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get target netns: %w", err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			logger.Warn("failed to restore original netns: %v", err)
		}
	}()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found in target netns: %w", ifName, err)
	}

	if newName != "" && newName != ifName {
		if err := netlink.LinkSetName(link, newName); err != nil {
			return fmt.Errorf("failed to rename interface: %w", err)
		}
		link, _ = netlink.LinkByName(newName)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up interface: %w", err)
	}

	return nil
}

// DeleteVeth deletes a veth interface.
func DeleteVeth(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Check if interface simply doesn't exist (not found)
		var linkNotFoundErr netlink.LinkNotFoundError
		if errors.As(err, &linkNotFoundErr) {
			return nil // Already gone, nothing to delete
		}
		// Check for syscall "no such device" error
		if errors.Is(err, syscall.ENODEV) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get veth %s: %w", name, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth %s: %w", name, err)
	}

	logger.Debug("Deleted veth %s", name)
	return nil
}

// SetupHostRouting sets up routing on the host side for internet access.
func SetupHostRouting(vethHost string, hostIP string) error {
	logger.Debug("Setting up host routing for %s with IP %s", vethHost, hostIP)

	link, err := netlink.LinkByName(vethHost)
	if err != nil {
		return fmt.Errorf("failed to get host veth: %w", err)
	}

	addr, err := netlink.ParseAddr(hostIP + "/30")
	if err != nil {
		return fmt.Errorf("failed to parse host IP: %w", err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		// Ignore if already exists
		logger.Debug("Warning: failed to add IP to host veth (may already exist): %v", err)
	}

	return nil
}

// SetupContainerRouting sets up routing inside the container namespace.
func SetupContainerRouting(nsPath string, ifName string, containerIP string, gatewayIP string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get current namespace
	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	// Switch to container namespace
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get target netns: %w", err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			logger.Warn("failed to restore original netns: %v", err)
		}
	}()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}

	addr, err := netlink.ParseAddr(containerIP + "/30")
	if err != nil {
		return fmt.Errorf("failed to parse container IP: %w", err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		logger.Debug("Warning: failed to add IP to container interface: %v", err)
	}

	// Add default route via gateway
	gw := parseIP(gatewayIP)
	if gw == nil {
		return fmt.Errorf("failed to parse gateway IP: %s", gatewayIP)
	}

	route := &netlink.Route{
		Gw: gw,
	}

	if err := netlink.RouteAdd(route); err != nil {
		logger.Debug("Warning: failed to add default route: %v", err)
	}

	logger.Debug("Container routing setup: %s via %s", containerIP, gatewayIP)
	return nil
}

// parseIP parses an IP string.
func parseIP(s string) []byte {
	parts := make([]byte, 4)
	var a, b, c, d int
	n, _ := fmt.Sscanf(s, "%d.%d.%d.%d", &a, &b, &c, &d)
	if n != 4 {
		return nil
	}
	parts[0] = byte(a)
	parts[1] = byte(b)
	parts[2] = byte(c)
	parts[3] = byte(d)
	return parts
}

// chainName is the custom iptables chain for tslink forwarding rules.
const chainName = "TSLINK-FORWARD"

// chainInitialized tracks if we've already set up the custom chain.
var (
	chainMu          sync.Mutex
	chainInitialized bool
)

// SetupNAT sets up MASQUERADE for traffic from the container.
// Uses a custom chain (TS-CNI-FORWARD) for organized rule management.
func SetupNAT(vethHost string) error {
	logger.Debug("Setting up NAT for %s", vethHost)

	// Enable IP forwarding (idempotent)
	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		logger.Warn("Failed to enable IP forwarding: %v", err)
	}

	// Initialize our custom chain and global rules (only once, with mutex protection)
	chainMu.Lock()
	if !chainInitialized {
		if err := initializeChain(); err != nil {
			chainMu.Unlock()
			return err
		}
		chainInitialized = true
	}
	chainMu.Unlock()

	// Allow forwarding for this specific veth (in our custom chain)
	cmd := exec.Command("iptables", "-A", chainName, "-i", vethHost, "-j", "ACCEPT")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("Failed to add FORWARD rule for %s: %v (%s)", vethHost, err, string(output))
	}

	cmd = exec.Command("iptables", "-A", chainName, "-o", vethHost, "-j", "ACCEPT")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("Failed to add FORWARD rule for %s: %v (%s)", vethHost, err, string(output))
	}

	logger.Debug("NAT setup complete for %s", vethHost)
	return nil
}

// initializeChain creates the custom chain and sets up global rules.
func initializeChain() error {
	// Create our custom chain (ignore error if already exists)
	cmd := exec.Command("iptables", "-N", chainName)
	if err := cmd.Run(); err != nil {
		logger.Debug("iptables chain %s may already exist: %v", chainName, err)
	}

	// Add jump rule from FORWARD to our chain (check first to avoid duplicates)
	cmd = exec.Command("iptables", "-C", "FORWARD", "-j", chainName)
	if cmd.Run() != nil {
		// Rule doesn't exist, add it at the beginning
		cmd = exec.Command("iptables", "-I", "FORWARD", "1", "-j", chainName)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to add jump to %s: %w (output: %s)", chainName, err, string(output))
		}
	}

	// Add global MASQUERADE rule for the 10.200.0.0/16 range
	cmd = exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", "10.200.0.0/16", "-j", "MASQUERADE")
	if cmd.Run() != nil {
		// Rule doesn't exist, add it
		cmd = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", "10.200.0.0/16", "-j", "MASQUERADE")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to add MASQUERADE rule: %w (output: %s)", err, string(output))
		}
	}

	logger.Info("Initialized iptables chain %s", chainName)
	return nil
}

// CleanupNAT removes the FORWARD rules for a specific veth interface.
// The global MASQUERADE rule and chain structure are intentionally left in place.
func CleanupNAT(vethHost string) error {
	logger.Debug("Cleaning up NAT rules for %s", vethHost)

	// Remove FORWARD rules from our custom chain (ignore errors if rules don't exist)
	cmd := exec.Command("iptables", "-D", chainName, "-i", vethHost, "-j", "ACCEPT")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("FORWARD -i rule not found for %s (already cleaned): %v (%s)", vethHost, err, string(output))
	}

	cmd = exec.Command("iptables", "-D", chainName, "-o", vethHost, "-j", "ACCEPT")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("FORWARD -o rule not found for %s (already cleaned): %v (%s)", vethHost, err, string(output))
	}

	logger.Debug("NAT cleanup complete for %s", vethHost)
	return nil
}

// CleanupAllNAT removes the entire custom chain and all its rules.
// This is useful for complete plugin cleanup.
func CleanupAllNAT() error {
	logger.Info("Cleaning up all NAT rules")

	// Remove jump rule from FORWARD
	cmd := exec.Command("iptables", "-D", "FORWARD", "-j", chainName)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("No jump rule to %s found (may already be cleaned): %v (%s)", chainName, err, string(output))
	}

	// Flush and delete our custom chain
	cmd = exec.Command("iptables", "-F", chainName)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("Failed to flush chain %s (may not exist): %v (%s)", chainName, err, string(output))
	}

	cmd = exec.Command("iptables", "-X", chainName)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("Failed to delete chain %s (may not exist): %v (%s)", chainName, err, string(output))
	}

	// Remove MASQUERADE rule
	cmd = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", "10.200.0.0/16", "-j", "MASQUERADE")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("No MASQUERADE rule found (may already be cleaned): %v (%s)", err, string(output))
	}

	chainMu.Lock()
	chainInitialized = false
	chainMu.Unlock()

	logger.Info("All NAT rules cleaned up")
	return nil
}
