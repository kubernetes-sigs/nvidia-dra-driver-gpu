/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	hostsFilePath = "/etc/hosts"
	dnsNamePrefix = "compute-domain-daemon-"
	dnsNameFormat = dnsNamePrefix + "%04d"
)

// IPToDNSNameMap holds a map of IP Addresses to DNS names.
type IPToDNSNameMap map[string]string

// DNSNameManager manages the allocation of static DNS names to IP addresses.
type DNSNameManager struct {
	sync.Mutex
	ipToDNSName           IPToDNSNameMap
	cliqueID              string
	maxNodesPerIMEXDomain int
	nodesConfigPath       string
}

// NewDNSNameManager creates a new DNS name manager.
func NewDNSNameManager(cliqueID string, maxNodesPerIMEXDomain int, nodesConfigPath string) *DNSNameManager {
	return &DNSNameManager{
		ipToDNSName:           make(IPToDNSNameMap),
		cliqueID:              cliqueID,
		maxNodesPerIMEXDomain: maxNodesPerIMEXDomain,
		nodesConfigPath:       nodesConfigPath,
	}
}

// UpdateDNSNameMappings updates the /etc/hosts file with any new IP to DNS name
// mappings. The boolean return value indicates whether the hosts file was
// updated or not (it must be ignored when the returned error is non-nil).
func (m *DNSNameManager) UpdateDNSNameMappings(daemons []*nvapi.ComputeDomainDaemonInfo) (bool, error) {
	m.Lock()
	defer m.Unlock()

	// Make a local ipToDNSName mappings
	ipToDNSName := make(IPToDNSNameMap)

	// Prefilter daemons to only consider those with the matching cliqueID
	var cliqueDaemons []*nvapi.ComputeDomainDaemonInfo
	for _, daemon := range daemons {
		if daemon.CliqueID == m.cliqueID {
			cliqueDaemons = append(cliqueDaemons, daemon)
		}
	}

	// Add IPs to map
	for _, daemon := range cliqueDaemons {
		// Construct the DNS name from the daemon index
		dnsName, err := m.constructDNSName(daemon)
		if err != nil {
			return false, fmt.Errorf("failed to construct DNS name for IP %s: %w", daemon.IPAddress, err)
		}

		// Assign the IP -> DNS name mapping
		ipToDNSName[daemon.IPAddress] = dnsName
	}

	// If the existing ipToDNSName mappings are unchanged, exit early
	if maps.Equal(ipToDNSName, m.ipToDNSName) {
		return false, nil
	}

	// Otherwise, update the cached ipToDNSName mapping
	m.ipToDNSName = ipToDNSName

	// And update the hosts file with the new mapping
	return true, m.updateHostsFile()
}

// LogDNSNameMappings logs the current compute-domain-daemon mappings from memory.
func (m *DNSNameManager) LogDNSNameMappings() {
	m.Lock()
	defer m.Unlock()

	if len(m.ipToDNSName) == 0 {
		klog.V(2).Infof("Current compute-domain-daemon mappings: empty")
		return
	}

	// Sort alphabetically by DNS name (map value) -> sort ips (map keys) based
	// on their corresponding values.
	var ips []string
	for ip := range m.ipToDNSName {
		ips = append(ips, ip)
	}

	slices.SortFunc(ips, func(a, b string) int {
		return cmp.Compare(m.ipToDNSName[a], m.ipToDNSName[b])
	})

	for _, ip := range ips {
		dnsname := m.ipToDNSName[ip]
		klog.V(2).Infof("%s -> %s", dnsname, ip)
	}
}

// constructDNSName constructs a DNS name for a daemon based on its index field.
// Returns an error if the index is invalid or exceeds maxNodesPerIMEXDomain.
func (m *DNSNameManager) constructDNSName(daemon *nvapi.ComputeDomainDaemonInfo) (string, error) {
	if daemon.Index < 0 {
		return "", fmt.Errorf("daemon %s has invalid index %d", daemon.NodeName, daemon.Index)
	}
	if daemon.Index >= m.maxNodesPerIMEXDomain {
		return "", fmt.Errorf("daemon %s has invalid index %d, must be less than %d", daemon.NodeName, daemon.Index, m.maxNodesPerIMEXDomain)
	}
	dnsName := fmt.Sprintf(dnsNameFormat, daemon.Index)
	return dnsName, nil
}

// updateHostsFile updates the /etc/hosts file with current IP to DNS name mappings.
func (m *DNSNameManager) updateHostsFile() error {
	// Read hosts file
	hostsContent, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", hostsFilePath, err)
	}

	// Grab any lines to preserve, skipping existing DNS name mappings
	var preservedLines []string
	for _, line := range strings.Split(string(hostsContent), "\n") {
		line = strings.TrimSpace(line)

		// Skip existing compute-domain-daemon mappings
		if strings.Contains(line, dnsNamePrefix) {
			continue
		}

		// Keep all other lines
		preservedLines = append(preservedLines, line)
	}

	// Add preserved lines
	var newHostsContent strings.Builder
	for _, line := range preservedLines {
		newHostsContent.WriteString(line)
		newHostsContent.WriteString("\n")
	}

	// Add a separator comment
	// TODO: do not write this more than once :-).
	newHostsContent.WriteString("# Compute Domain Daemon mappings\n")

	// Add new DNS name mappings
	for ip, dnsName := range m.ipToDNSName {
		_, _ = fmt.Fprintf(&newHostsContent, "%s\t%s\n", ip, dnsName)
	}

	// Write the updated hosts file
	if err := os.WriteFile(hostsFilePath, []byte(newHostsContent.String()), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", hostsFilePath, err)
	}

	return nil
}

// WriteNodesConfig creates a static nodes config file with DNS names.
func (m *DNSNameManager) WriteNodesConfig() error {
	// Ensure the directory exists
	dir := filepath.Dir(m.nodesConfigPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Create or overwrite the nodesConfig file
	f, err := os.Create(m.nodesConfigPath)
	if err != nil {
		return fmt.Errorf("failed to create nodes config file: %w", err)
	}
	defer f.Close()

	// Write static DNS names
	for i := 0; i < m.maxNodesPerIMEXDomain; i++ {
		dnsName := fmt.Sprintf(dnsNameFormat, i)
		if _, err := fmt.Fprintf(f, "%s\n", dnsName); err != nil {
			return fmt.Errorf("failed to write to nodes config file: %w", err)
		}
	}

	klog.Infof("Created static nodes config file with %d DNS names using format %s", m.maxNodesPerIMEXDomain, dnsNameFormat)

	return nil
}
