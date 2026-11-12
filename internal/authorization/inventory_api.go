package authorization

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

const (
	inventoryAPIGroup   = "inventory.pve.edorid.github.io"
	inventoryAPIVersion = "v1alpha1"
	inventoryAPIBase    = "/apis/" + inventoryAPIGroup + "/" + inventoryAPIVersion
)

type inventoryDiscover func(context.Context, cloudCredential) (any, error)
type inventoryAllowedHosts func(context.Context) ([]string, error)

type inventoryAPI struct {
	kubernetes   *kubeClient
	discover     inventoryDiscover
	allowedHosts inventoryAllowedHosts
}

func (api inventoryAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if _, err := identityFromAggregatedRequest(request); err != nil {
		http.Error(writer, `{"error":"authenticated request required"}`, http.StatusUnauthorized)
		return
	}

	switch request.URL.Path {
	case "/apis/" + inventoryAPIGroup:
		writeJSON(writer, http.StatusOK, map[string]any{
			"kind": "APIGroup", "apiVersion": "v1", "name": inventoryAPIGroup,
			"preferredVersion": map[string]string{"groupVersion": inventoryAPIGroup + "/" + inventoryAPIVersion, "version": inventoryAPIVersion},
			"versions":         []map[string]string{{"groupVersion": inventoryAPIGroup + "/" + inventoryAPIVersion, "version": inventoryAPIVersion}},
		})
		return
	case inventoryAPIBase:
		writeJSON(writer, http.StatusOK, map[string]any{
			"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": inventoryAPIGroup + "/" + inventoryAPIVersion,
			"resources": []map[string]any{{
				"name": "proxmoxinventories", "singularName": "proxmoxinventory", "namespaced": true,
				"kind": "ProxmoxInventory", "verbs": []string{"get"},
			}},
		})
		return
	}

	const routePrefix = inventoryAPIBase + "/namespaces/" + cloudCredentialNamespace + "/proxmoxinventories/"
	if !strings.HasPrefix(request.URL.Path, routePrefix) {
		http.NotFound(writer, request)
		return
	}
	name := strings.TrimPrefix(request.URL.Path, routePrefix)
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(writer, request)
		return
	}
	identity, err := identityFromAggregatedRequest(request)
	if err != nil {
		http.Error(writer, `{"error":"authenticated request required"}`, http.StatusUnauthorized)
		return
	}
	credential, err := api.kubernetes.getCloudCredential(request.Context(), identity, name)
	if err != nil {
		http.Error(writer, `{"error":"credential access denied or unavailable"}`, http.StatusForbidden)
		return
	}
	target, err := validateProxmoxURL(credential.URL)
	if err != nil {
		http.Error(writer, `{"error":"credential URL is invalid"}`, http.StatusBadRequest)
		return
	}
	allowedHosts := api.allowedHosts
	if allowedHosts == nil {
		allowedHosts = api.kubernetes.proxmoxAllowedHosts
	}
	approvedHosts, err := allowedHosts(request.Context())
	if err != nil {
		http.Error(writer, `{"error":"Proxmox host approval state is unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if !isProxmoxHostAllowed(target.Hostname(), approvedHosts) {
		writeJSON(writer, http.StatusForbidden, map[string]string{
			"error": "proxmox_host_not_allowed",
			"host":  target.Hostname(),
		})
		return
	}
	if api.discover == nil {
		http.Error(writer, `{"error":"inventory discovery unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	inventory, err := api.discover(request.Context(), credential)
	if err != nil {
		// Discovery errors are deliberately sanitized by the production discoverer.
		// Do not log upstream response bodies, URLs, or credential material.
		log.Printf("inventory API: Proxmox discovery failed: %s", err.Error())
		http.Error(writer, `{"error":"Proxmox inventory discovery failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion":  inventoryAPIGroup + "/" + inventoryAPIVersion,
		"kind":        "ProxmoxInventory",
		"metadata":    map[string]string{"name": name, "namespace": cloudCredentialNamespace},
		"insecureTLS": credential.InsecureTLS,
		"inventory":   inventory,
	})
}

func (c *kubeClient) proxmoxAllowedHosts(ctx context.Context) ([]string, error) {
	const nodeDriverPath = "/apis/management.cattle.io/v3/nodedrivers/pve"
	data, err := c.request(ctx, http.MethodGet, nodeDriverPath, nil)
	if err != nil {
		return nil, errors.New("Proxmox node driver policy is unavailable")
	}
	var nodeDriver struct {
		Spec struct {
			WhitelistDomains []string `json:"whitelistDomains"`
		} `json:"spec"`
	}
	if json.Unmarshal(data, &nodeDriver) != nil {
		return nil, errors.New("Proxmox node driver policy is invalid")
	}
	approved := append([]string(nil), nodeDriver.Spec.WhitelistDomains...)
	for _, host := range strings.Split(os.Getenv("PROXMOX_ALLOWED_HOSTS"), ",") {
		if host = strings.TrimSpace(host); host != "" {
			approved = append(approved, host)
		}
	}
	return approved, nil
}

func isProxmoxHostAllowed(host string, approved []string) bool {
	for _, allowed := range approved {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

type proxmoxInventory struct {
	ResourcePools []string        `json:"resourcePools"`
	Nodes         []inventoryNode `json:"nodes"`
}

type inventoryNode struct {
	Name      string              `json:"name"`
	Online    bool                `json:"online"`
	Bridges   []string            `json:"bridges"`
	Storages  []inventoryStorage  `json:"storages,omitempty"`
	Templates []inventoryTemplate `json:"templates,omitempty"`
	Error     string              `json:"error,omitempty"`
}

type inventoryStorage struct {
	ID     string `json:"id"`
	Ready  bool   `json:"ready"`
	Images bool   `json:"images"`
	Shared bool   `json:"shared"`
}

type inventoryTemplate struct {
	VMID                 uint64            `json:"vmid"`
	Name                 string            `json:"name"`
	Disks                map[string]string `json:"disks"`
	DiskStorages         map[string]string `json:"diskStorages"`
	Boot                 string            `json:"boot"`
	BootDisk             string            `json:"bootDisk"`
	Networks             map[string]string `json:"networks"`
	IPConfigs            map[string]string `json:"ipConfigs"`
	NetworkInterface     string            `json:"networkInterface"`
	NetworkBridge        string            `json:"networkBridge"`
	DynamicAddress       bool              `json:"dynamicAddress"`
	CloudInit            bool              `json:"cloudInit"`
	GuestAgent           bool              `json:"guestAgent"`
	CompatibilityReasons []string          `json:"compatibilityReasons"`
}

func discoverProxmoxInventory(ctx context.Context, credential cloudCredential) (any, error) {
	target, err := validateProxmoxURL(credential.URL)
	if err != nil {
		return nil, errors.New("Proxmox credential URL is invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: credential.InsecureTLS} //nolint:gosec // Explicit per-credential opt-in, displayed with a warning in the UI.
	httpClient := &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	client := proxmox.NewClient(target.JoinPath("api2/json").String(),
		proxmox.WithAPIToken(credential.TokenID, credential.TokenSecret), proxmox.WithHTTPClient(httpClient))
	nodes, err := client.Nodes(ctx)
	if errors.Is(err, proxmox.ErrNotAuthorized) {
		log.Print("inventory API: Proxmox rejected the configured API token")
		return nil, errors.New("Proxmox node inventory is unavailable")
	}
	if err != nil {
		log.Printf("inventory API: Proxmox nodes request failed with error type %T", err)
		return nil, errors.New("Proxmox node inventory is unavailable")
	}
	if len(nodes) == 0 {
		return nil, errors.New("Proxmox node inventory is unavailable")
	}
	pools, err := client.Pools(ctx)
	if err != nil {
		return nil, errors.New("Proxmox resource pool inventory is unavailable")
	}
	result := proxmoxInventory{ResourcePools: make([]string, 0, len(pools)), Nodes: make([]inventoryNode, 0, len(nodes))}
	for _, pool := range pools {
		if pool == nil || pool.PoolID == "" {
			return nil, errors.New("Proxmox resource pool inventory is incomplete")
		}
		result.ResourcePools = append(result.ResourcePools, pool.PoolID)
	}
	for _, status := range nodes {
		if status == nil || status.Node == "" {
			return nil, errors.New("Proxmox node inventory is incomplete")
		}
		item := inventoryNode{Name: status.Node, Online: status.Status == "online" || status.Online == 1}
		if item.Online {
			if err := discoverNodeInventory(ctx, client, status.Node, &item); err != nil {
				// Keep host visibility but mark this host as partially unavailable.
				item.Error = "Unable to read complete host inventory. Retry discovery."
				item.Storages, item.Bridges, item.Templates = nil, nil, nil
			}
		}
		result.Nodes = append(result.Nodes, item)
	}
	return result, nil
}

func discoverNodeInventory(ctx context.Context, client *proxmox.Client, nodeName string, result *inventoryNode) error {
	node, err := client.Node(ctx, nodeName)
	if err != nil {
		return err
	}
	storages, err := node.Storages(ctx)
	if err != nil {
		return err
	}
	for _, storage := range storages {
		if storage == nil || storage.Name == "" {
			return errors.New("invalid storage inventory")
		}
		result.Storages = append(result.Storages, inventoryStorage{
			ID: storage.Name, Ready: storage.Enabled == 1 && storage.Active == 1,
			Images: hasContentType(storage.Content, "images"), Shared: storage.Shared == 1,
		})
	}
	storageMap := make(map[string]inventoryStorage, len(result.Storages))
	for _, storage := range result.Storages {
		storageMap[storage.ID] = storage
	}
	networks, err := node.Networks(ctx)
	if err != nil {
		return err
	}
	bridgeSet := make(map[string]bool)
	for _, network := range networks {
		if network != nil && network.Type == "bridge" && network.Iface != "" {
			bridgeSet[network.Iface] = true
			result.Bridges = append(result.Bridges, network.Iface)
		}
	}
	vms, err := node.VirtualMachines(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if vm == nil || !bool(vm.Template) {
			continue
		}
		if vm.VMID > 999999999 {
			return errors.New("template VMID is out of range")
		}
		template, err := node.VirtualMachine(ctx, int(vm.VMID))
		if err != nil || template == nil || template.VirtualMachineConfig == nil {
			return fmt.Errorf("template config unavailable")
		}
		config := template.VirtualMachineConfig
		disks := config.MergeDisks()
		if config.EFIDisk0 != "" {
			disks["efidisk0"] = config.EFIDisk0
		}
		if config.TPMState0 != "" {
			disks["tpmstate0"] = config.TPMState0
		}
		networkDevices, ipConfigs := config.MergeNets(), config.MergeIPConfigs()
		templateInventory := inventoryTemplate{
			VMID: uint64(vm.VMID), Name: vm.Name, Disks: disks, DiskStorages: make(map[string]string), Boot: config.Boot,
			Networks: networkDevices, IPConfigs: ipConfigs,
			CloudInit: hasCloudInitDrive(config), GuestAgent: config.Agent == "1" || strings.Contains(config.Agent, "enabled=1"),
			CompatibilityReasons: []string{},
		}
		for device, value := range disks {
			if storage, found := templateStorageID(value); found {
				templateInventory.DiskStorages[device] = storage
				if value != "none" && !strings.Contains(value, "media=cdrom") {
					storageInfo, available := storageMap[storage]
					if !available {
						templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "a required disk storage is unavailable on this host")
					} else if storageInfo.Shared {
						templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "a required disk uses shared storage")
					}
				}
			} else if value != "none" && !strings.Contains(value, "media=cdrom") {
				templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "a required disk has no identifiable storage")
			}
		}
		templateInventory.BootDisk = findBootDisk(config.Boot, disks)
		if templateInventory.BootDisk == "" {
			templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "boot disk is not identifiable")
		}
		templateInventory.NetworkInterface, templateInventory.NetworkBridge, templateInventory.DynamicAddress = inferTemplateNetwork(networkDevices, ipConfigs, bridgeSet)
		if templateInventory.NetworkInterface == "" {
			templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "network interface is missing or ambiguous")
		} else if templateInventory.NetworkBridge == "" {
			templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "network bridge is missing or unavailable on this host")
		}
		if !templateInventory.DynamicAddress {
			templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "dynamic cloud-init addressing cannot be verified")
		}
		if !templateInventory.CloudInit {
			templateInventory.CompatibilityReasons = append(templateInventory.CompatibilityReasons, "cloud-init drive is missing")
		}
		result.Templates = append(result.Templates, templateInventory)
	}
	return nil
}

func templateStorageID(device string) (string, bool) {
	volume := strings.SplitN(device, ",", 2)[0]
	storage, _, found := strings.Cut(volume, ":")
	return storage, found && storage != ""
}

func findBootDisk(boot string, disks map[string]string) string {
	_, order, found := strings.Cut(boot, "order=")
	if !found {
		return ""
	}
	order, _, _ = strings.Cut(order, ",")
	for _, name := range strings.Split(order, ";") {
		value, found := disks[name]
		if found && value != "none" && !strings.Contains(value, "media=cdrom") && !strings.Contains(value, "cloudinit") {
			return name
		}
	}
	return ""
}

func inferTemplateNetwork(devices, ipConfigs map[string]string, bridges map[string]bool) (string, string, bool) {
	name := ""
	if devices["net0"] != "" {
		name = "net0"
	} else if len(devices) == 1 {
		for candidate := range devices {
			name = candidate
		}
	} else {
		return "", "", false
	}
	bridge := deviceOption(devices[name], "bridge")
	number, err := strconv.Atoi(strings.TrimPrefix(name, "net"))
	if err != nil || number < 0 || number > 31 {
		return name, "", false
	}
	address, address6 := "", ""
	for _, option := range strings.Split(ipConfigs["ipconfig"+strconv.Itoa(number)], ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(option), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			address = value
		case "ip6":
			address6 = value
		}
	}
	return name, bridgeIfPresent(bridge, bridges), address == "dhcp" && (address6 == "" || address6 == "dhcp")
}

func deviceOption(device, optionName string) string {
	for _, option := range strings.Split(device, ",") {
		key, value, found := strings.Cut(option, "=")
		if found && key == optionName {
			return value
		}
	}
	return ""
}

func bridgeIfPresent(bridge string, available map[string]bool) string {
	if available[bridge] {
		return bridge
	}
	return ""
}

func validateProxmoxURL(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" ||
		(target.Scheme != "https" && target.Scheme != "http") || (target.Path != "" && target.Path != "/") {
		return nil, errors.New("invalid Proxmox URL")
	}
	if net.ParseIP(target.Hostname()) == nil && strings.ContainsAny(target.Hostname(), " /\\\r\n\t") {
		return nil, errors.New("invalid Proxmox host")
	}
	if port := target.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, errors.New("invalid Proxmox port")
		}
	}
	return target, nil
}

func hasContentType(content, expected string) bool {
	for _, value := range strings.Split(content, ",") {
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}

func hasCloudInitDrive(config *proxmox.VirtualMachineConfig) bool {
	for _, device := range config.MergeDisks() {
		if strings.Contains(device, "cloudinit") {
			return true
		}
	}
	return false
}
