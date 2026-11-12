package driver

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/edorid/docker-machine-driver-pve/internal/placement"
	"github.com/rancher/machine/libmachine/drivers"
)

const gibibyte = uint64(1 << 30)

var linuxUserName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

//nolint:cyclop,wsl // Parse and validate placement options at one fail-closed boundary.
func (d *Driver) setPlacementConfig(opts drivers.DriverOptions) error {
	d.PlacementGroup = strings.TrimSpace(opts.String(flagPlacementGroup))
	d.PlacementHosts = splitPlacementList(opts.String(flagPlacementHosts))
	d.PlacementStorages = splitPlacementList(opts.String(flagPlacementStorage))

	d.PlacementLedgerNamespace = strings.TrimSpace(opts.String(flagLedgerNamespace))
	d.PlacementLedgerName = strings.TrimSpace(opts.String(flagLedgerName))
	d.PlacementClusterID = strings.TrimSpace(opts.String(flagClusterID))
	values := []string{
		d.PlacementGroup, opts.String(flagPlacementHosts), opts.String(flagPlacementStorage),
		opts.String(flagPlacementTpls), opts.String(flagDiskSize), d.PlacementLedgerNamespace,
		d.PlacementLedgerName, d.PlacementClusterID,
	}
	configured := false
	for _, value := range values {
		configured = configured || strings.TrimSpace(value) != ""
	}
	if !configured {
		return nil
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return errors.New("placement mode requires group, hosts, storages, per-host templates, and boot disk size")
		}
	}

	var err error
	d.PlacementTemplates, err = parsePlacementTemplates(opts.String(flagPlacementTpls))
	if err != nil {
		return fmt.Errorf("parse placement templates: %w", err)
	}
	d.DiskSizeGiB, err = strconv.ParseUint(strings.TrimSpace(opts.String(flagDiskSize)), 10, 32)
	if err != nil || d.DiskSizeGiB == 0 || d.DiskSizeGiB > math.MaxUint64/gibibyte {
		return errors.New("placement boot disk size must be a positive GiB value")
	}
	if d.ProcessorSockets == nil || d.ProcessorCores == nil || d.Memory == nil {
		return errors.New("placement mode requires explicit processor sockets, cores, and memory")
	}
	policy := placement.Policy{
		GroupID: d.PlacementGroup, AllowedHosts: d.PlacementHosts,
		AllowedStorages: d.PlacementStorages, Templates: d.PlacementTemplates,
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid placement policy: %w", err)
	}
	if !dnsLabel(d.PlacementLedgerNamespace) || !dnsLabel(d.PlacementLedgerName) || !printableIdentity(d.PlacementClusterID) {
		return errors.New("placement ledger namespace/name and cluster ID are invalid")
	}
	if _, err := networkInterfaceNumber(d.NetworkInterfaceName); err != nil {
		return err
	}
	if !linuxUserName.MatchString(d.SSHUser) {
		return errors.New("placement SSH user must be a valid Linux account name")
	}
	return nil
}

//nolint:wsl // Keep compact DNS label validation in one helper.
func dnsLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	last := value[len(value)-1]
	return last >= 'a' && last <= 'z' || last >= '0' && last <= '9'
}

//nolint:wsl // Printable identity validation is intentionally linear.
func printableIdentity(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < ' ' || char > '~' {
			return false
		}
	}
	return true
}

//nolint:wsl // Normalize list values in a single pass.
func splitPlacementList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

//nolint:wsl // Parse each host mapping as a single validated unit.
func parsePlacementTemplates(value string) (map[string]int, error) {
	result := make(map[string]int)
	for _, entry := range splitPlacementList(value) {
		parts := strings.Split(entry, "=")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("each mapping must be host=VMID")
		}
		vmid, err := strconv.Atoi(parts[1])
		if err != nil || vmid < 100 || vmid > 999999999 {
			return nil, errors.New("template VMIDs must be integers in [100,999999999]")
		}
		if _, exists := result[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate template mapping for host %q", parts[0])
		}
		result[parts[0]] = vmid
	}
	return result, nil
}
