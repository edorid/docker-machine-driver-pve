<script>
import { SECRET } from '@shell/config/types';
import { LabeledInput } from '@components/Form/LabeledInput';
import LabeledSelect from '@shell/components/form/LabeledSelect';
import UnitInput from '@shell/components/form/UnitInput';
import Checkbox from '@components/Form/Checkbox/Checkbox';

export default {
  components: {
    LabeledInput,
    LabeledSelect,
    UnitInput,
    Checkbox,
  },
  props: {
    uuid: {
      type:     String,
      required: true,
    },
    mode: {
      type:     String,
      required: true,
    },
    value: {
      type:     Object,
      required: true,
    },
    credentialId: {
      type:     String,
      required: true,
    },
  },
  data(){
    return {
      // Count of currently running external data fetches.
      fetchingCount: 0,

      // Cloud credential data.
      credential: null,

      // Available Proxmox VE resource pools.
      resourcePools: null,

      // Available Proxmox VE templates in current resource pool.
      templates: null,

      // Available devices on current Proxmox VE template.
      devices: null,
      placementInventory: null,
      placementInventoryError: '',
      placementInventoryInsecureTLS: false,
      placementInventoryRequestId: 0,
      placementInventoryBlockedHost: '',
      placementInventoryAllowlistBusy: false,
      selectedPlacementHosts: (this.value.placementHosts || '').split(',').map(value => value.trim()).filter(Boolean),
      selectedPlacementStorages: (this.value.placementStorages || '').split(',').map(value => value.trim()).filter(Boolean),
      placementTemplateSelections: (this.value.placementTemplates || '').split(',').reduce((result, entry) => {
        const [host, vmid] = entry.split('=');
        if(host && vmid) {
          result[host.trim()] = vmid.trim();
        }
        return result;
      }, {}),
      memoryBallooningEnabled: this.value.memoryBalloon == null || this.value.memoryBalloon === ''
        ? !((this.value.placementGroup != null && this.value.placementGroup !== '') || this.mode === 'create')
        : Number(this.value.memoryBalloon) > 0,

      // Current input values.
      currentValue: {
        resourcePool: this.value.resourcePool ?? '',
        template: this.value.template ? parseInt(this.value.template) : 0,
        isoDevice: this.value.isoDevice ?? '',
        networkInterface: this.value.networkInterface ?? '',
        sshUser: this.value.sshUser ? this.value.sshUser : 'user',
        sshPort: this.value.sshPort ? parseInt(this.value.sshPort) : 22,
        processorSockets: this.value.processorSockets ? parseInt(this.value.processorSockets) : 1,
        processorCores: this.value.processorCores ? parseInt(this.value.processorCores) : "",
        memory: this.value.memory ? parseInt(this.value.memory) : "",
        memoryBalloon: this.value.memoryBalloon ? parseInt(this.value.memoryBalloon) : "",
        fullClone: this.value.fullClone ?? true,
        tags: this.value.tags ?? '',
        placementEnabled: (this.value.placementGroup != null && this.value.placementGroup !== '') || this.mode === 'create',
        placementGroup: this.value.placementGroup || (this.mode === 'create' ? `pve-placement-${this.uuid}` : ''),
        placementHosts: this.value.placementHosts ?? '',
        placementStorages: this.value.placementStorages ?? '',
        placementTemplates: this.value.placementTemplates ?? '',
        diskSizeGiB: this.value.diskSizeGib ?? '',
        placementLedgerNamespace: this.value.placementLedgerNamespace || 'fleet-default',
        placementLedgerName: this.value.placementLedgerName ?? '',
        placementClusterId: this.value.placementClusterId ?? '',
        placementGuestReady: this.value.placementGuestReady ?? false,
        cloudInitNetworkSnippet: this.value.cloudInitNetworkSnippet ?? '',
        rancherClusterName: this.value.rancherClusterName ?? '',
        rancherMachinePool: this.value.rancherMachinePool ?? '',
      },
    }
  },
  created(){
    this.validate();
  },
  async fetch(){
    if(this.currentValue.placementEnabled) {
      await this.fetchPlacementInventory();
      return;
    }
    await this.fetchCredential();
    await this.fetchResourcePools();
    await this.fetchTemplates();
    await this.fetchDevices();
  },
  watch: {
    async credentialId(){
      if(this.currentValue.placementEnabled) {
        await this.fetchPlacementInventory();
      } else {
        await this.$fetch();
      }
      this.validate();
    },
    'currentValue.placementEnabled': async function(enabled) {
      if(enabled) {
        if(!this.currentValue.placementGroup) {
          this.currentValue.placementGroup = `pve-placement-${this.uuid}`;
        }
        this.credential = null;
        this.placementInventory = null;
        await this.fetchPlacementInventory();
      } else {
        await this.$fetch();
      }
    },
    'currentValue.resourcePool': async function (){
      await this.fetchTemplates();
      await this.fetchDevices();
    },
    'currentValue.template': async function (){
      await Promise.all([
        this.fetchDevices(),
        this.fetchHardware(),
      ]);
    },
    selectedPlacementHosts(hosts) {
      this.currentValue.placementHosts = hosts.join(',');
      const allowed = new Set(hosts);
      this.placementTemplateSelections = Object.fromEntries(Object.entries(this.placementTemplateSelections).filter(([host]) => allowed.has(host)));
      this.syncPlacementTemplateMap();
    },
    selectedPlacementStorages(storages) {
      this.currentValue.placementStorages = storages.join(',');
    },
    'currentValue.memory': function(newValue, oldValue) {
      if(!this.memoryBallooningEnabled || this.currentValue.memoryBalloon == 0) {
        return
      }

      if(oldValue == this.currentValue.memoryBalloon || newValue < this.currentValue.memoryBalloon) {
        this.currentValue.memoryBalloon = newValue;
      }
    },
    currentValue: {
      deep: true,
      handler(){
        this.validate();
      },
    },
    resourcePools(){
      if(this.resourcePools == null) {
        return
      }

      if(this.resourcePools.length == 1) {
        this.currentValue.resourcePool = this.resourcePools[0];
      }

      if(!this.resourcePools.includes(this.currentValue.resourcePool)) {
        this.currentValue.resourcePool = '';
      }
    },
    templates(){
      if(this.templates == null) {
        return
      }

      if(this.templates.length == 1) {
        this.currentValue.template = this.templates[0].vmid;
      }

      const currentTemplate = this.templates.find(template => this.currentValue.template == template.vmid)

      if(!currentTemplate) {
        this.currentValue.template = 0;
      }
    },
    isoDeviceSelectOptions(){
      if(this.isoDeviceSelectOptions == null) {
        return
      }

      if(this.isoDeviceSelectOptions.length == 1) {
        this.currentValue.isoDevice = this.isoDeviceSelectOptions[0];
      }

      if(!this.isoDeviceSelectOptions.includes(this.currentValue.isoDevice)) {
        this.currentValue.isoDevice = '';
      }
    },
    networkInterfaceSelectOptions(){
      if(this.networkInterfaceSelectOptions == null) {
        return
      }

      if(this.networkInterfaceSelectOptions.length == 1) {
        this.currentValue.networkInterface = this.networkInterfaceSelectOptions[0];
      }

      if(!this.networkInterfaceSelectOptions.includes(this.currentValue.networkInterface)) {
        this.currentValue.networkInterface = '';
      }
    },
  },
  methods: {
    validate(){
      // Default optional number fields to empty string
      if(this.currentValue.processorSockets === null || this.currentValue.processorSockets === 0) {
        this.currentValue.processorSockets = "";
      }

      if(this.currentValue.processorCores === null || this.currentValue.processorCores === 0) {
        this.currentValue.processorCores = "";
      }

      if(this.currentValue.memory === null || this.currentValue.memory === 0) {
        this.currentValue.memory = "";
      }

      if(this.currentValue.memoryBalloon === null) {
        this.currentValue.memoryBalloon = "";
      }

      // Sanity check current value
      if(this.currentValue.resourcePool == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(!this.currentValue.placementEnabled && this.currentValue.template < 1) {
        this.$emit('validationChanged', false);
        return
      }

      if(!this.currentValue.placementEnabled && this.currentValue.isoDevice == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(!this.currentValue.placementEnabled && this.currentValue.networkInterface == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.sshUser == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.sshPort < 1) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.processorSockets != "" && this.currentValue.processorSockets < 1) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.processorCores != "" && this.currentValue.processorCores < 1) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.memory != "" && this.currentValue.memory < 1) {
        this.$emit('validationChanged', false);
        return
      }

      if(!this.currentValue.placementEnabled && this.currentValue.memoryBalloon != "" && this.currentValue.memoryBalloon < 0) {
        this.$emit('validationChanged', false);
        return
      }

      if(!this.currentValue.placementEnabled && this.currentValue.memory != "" && this.currentValue.memoryBalloon != "" && this.currentValue.memory < this.currentValue.memoryBalloon) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.placementEnabled && (
        !this.currentValue.placementGroup.trim() || !this.currentValue.placementHosts.trim() ||
        !this.currentValue.placementStorages.trim() || !this.currentValue.placementTemplates.trim() ||
        !this.placementTemplatesValid || this.selectedPlacementHosts.some(host => !this.placementTemplateCompatible(host)) ||
        !this.currentValue.diskSizeGiB || Number(this.currentValue.diskSizeGiB) < 1 ||
        !this.currentValue.placementLedgerNamespace.trim() || !this.currentValue.placementLedgerName.trim() ||
        !this.currentValue.placementClusterId.trim() || this.currentValue.processorSockets < 1 ||
        this.currentValue.processorCores < 1 || this.currentValue.memory < 1 || !this.currentValue.placementGuestReady
      )) {
        this.$emit('validationChanged', false);
        return
      }

      // Copy current value to the 'value' prop
      this.value.resourcePool = this.currentValue.resourcePool;
      this.value.template = this.currentValue.placementEnabled ? '' : this.currentValue.template.toString();
      this.value.isoDevice = this.currentValue.placementEnabled ? '' : this.currentValue.isoDevice;
      this.value.networkInterface = this.currentValue.placementEnabled ? '' : this.currentValue.networkInterface;
      this.value.sshUser = this.currentValue.sshUser;
      this.value.sshPort = this.currentValue.sshPort.toString();
      this.value.processorSockets = this.currentValue.processorSockets.toString();
      this.value.processorCores = this.currentValue.processorCores.toString();
      this.value.memory = this.currentValue.memory.toString();
      this.value.memoryBalloon = this.memoryBallooningEnabled
        ? (this.currentValue.memory || '').toString()
        : '0';
      this.value.fullClone = this.currentValue.fullClone;
      this.value.placementGroup = this.currentValue.placementEnabled ? this.currentValue.placementGroup.trim() : '';
      this.value.placementHosts = this.currentValue.placementEnabled ? this.currentValue.placementHosts.trim() : '';
      this.value.placementStorages = this.currentValue.placementEnabled ? this.currentValue.placementStorages.trim() : '';
      this.value.placementTemplates = this.currentValue.placementEnabled ? this.currentValue.placementTemplates.trim() : '';
      this.value.diskSizeGib = this.currentValue.placementEnabled ? this.currentValue.diskSizeGiB.toString() : '';
      this.value.placementLedgerNamespace = this.currentValue.placementEnabled ? this.currentValue.placementLedgerNamespace.trim() : '';
      this.value.placementLedgerName = this.currentValue.placementEnabled ? this.currentValue.placementLedgerName.trim() : '';
      this.value.placementClusterId = this.currentValue.placementEnabled ? this.currentValue.placementClusterId.trim() : '';
      this.value.placementGuestReady = this.currentValue.placementEnabled ? this.currentValue.placementGuestReady : false;
      this.value.cloudInitNetworkSnippet = this.currentValue.placementEnabled ? this.currentValue.cloudInitNetworkSnippet.trim() : '';
      this.value.rancherClusterName = this.currentValue.rancherClusterName.trim();
      this.value.rancherMachinePool = this.currentValue.rancherMachinePool.trim();

      this.$emit('validationChanged', true);
    },
    async fetchCredential(){
      try {
        this.fetchingCount += 1;

        const secret = await this.$store.dispatch('management/find', {
          type: SECRET,
          id: this.credentialId.replace(':', '/'),
        });

        const decodedData = {
          url: atob(secret.data['pvecredentialConfig-url']),
          insecureTls: atob(secret.data['pvecredentialConfig-insecureTls']) == 'true',
          tokenId: atob(secret.data['pvecredentialConfig-tokenId']),
          tokenSecret: atob(secret.data['pvecredentialConfig-tokenSecret']),
        }

        if(decodedData.insecureTls) {
          // If insecure TLS is enabled then we don't have ability to call Proxmox VE from the UI.
          throw new Error('Insecure TLS is enabled')
        }
        
        this.credential = decodedData;
      } catch(e) {
        console.warn("Failed to fetch Proxmox VE credential, enhanced form fields are disabled", e)
        this.credential = null;
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async fetchResourcePools(){
      try {
        this.fetchingCount += 1;

        const { data } = await this.fetchFromProxmox('/api2/json/pools');

        this.resourcePools = data.map(pool => pool.poolid);
      } catch(e) {
        this.resourcePools = null;
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async fetchTemplates(){
      if(this.credential == null) {
        this.templates = null;
        return;
      }

      if(this.resourcePools && this.currentValue.resourcePool == '') {
        this.templates = [];
        return;
      }

      try {
        this.fetchingCount += 1;

        if(!this.currentValue.resourcePool) {
          throw new Error('Resource pool is not selected')
        }

        const { data } = await this.fetchFromProxmox(`/api2/json/pools?type=qemu&poolid=${this.currentValue.resourcePool}`);
        
        this.templates = data[0].members.filter(vm => vm.template == 1);
      } catch(e) {
        this.templates = null;
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async fetchDevices(){
      if(this.credential == null) {
        this.devices = null;
        return;
      }

      if(this.templates && this.currentValue.template == '') {
        this.devices = [];
        return;
      }

      try {
        this.fetchingCount += 1;

        if(!this.currentValue.template) {
          throw new Error('Template is not selected');
        }

        const template = this.templates.find(template => this.currentValue.template == template.vmid);

        if(!template) {
          throw new Error("Template not found")
        }

        const { data } = await this.fetchFromProxmox(`/api2/json/nodes/${template.node}/qemu/${template.vmid}/pending`);
        
        this.devices = data;
      } catch(e) {
        this.devices = null;
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async fetchHardware() {
      if(this.credential == null) {
        return;
      }

      if(this.templates && this.currentValue.template == '') {
        return;
      }

      try {
        this.fetchingCount += 1;

        if(!this.currentValue.template) {
          throw new Error('Template is not selected');
        }

        const template = this.templates.find(template => this.currentValue.template == template.vmid);

        if(!template) {
          throw new Error("Template not found")
        }

        const { data } = await this.fetchFromProxmox(`/api2/json/nodes/${template.node}/qemu/${template.vmid}/config`);

        this.currentValue.processorSockets = data.sockets;
        this.currentValue.processorCores = data.cores;
        this.currentValue.memory = data.memory;
        // Memory balloon is an advanced (hidden by default) field, so we default to using the memory value so it is synced unless set explicitly to a different value.
        this.currentValue.memoryBalloon = data.memory;
      } catch(e) {
        this.currentValue.processorSockets = "";
        this.currentValue.processorCores = "";
        this.currentValue.memory = "";
        this.currentValue.memoryBalloon = "";
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async fetchPlacementInventory() {
      const requestId = ++this.placementInventoryRequestId;
      this.currentValue.placementGuestReady = false;
      this.placementInventoryError = '';
      this.placementInventoryBlockedHost = '';
      this.placementInventory = null;
      this.placementInventoryInsecureTLS = false;
      this.currentValue.placementClusterId = '';
      this.currentValue.placementLedgerName = '';
      this.resourcePools = null;
      if(!this.credentialId) {
        this.placementInventoryError = 'Select a Proxmox cloud credential to discover inventory.';
        return;
      }
      const parts = this.credentialId.includes(':') ? this.credentialId.split(':') : this.credentialId.split('/');
      const [namespace, credentialName, extra] = parts;
      if(namespace !== 'cattle-global-data' || !credentialName || extra !== undefined) {
        this.placementInventoryError = 'The selected credential is not a supported Proxmox cloud credential.';
        return;
      }
      try {
        this.fetchingCount += 1;
        const inventoryResponse = await this.$store.dispatch('management/request', {
          method: 'GET',
          url: `/k8s/clusters/local/apis/inventory.pve.edorid.github.io/v1alpha1/namespaces/cattle-global-data/proxmoxinventories/${encodeURIComponent(credentialName)}`,
          redirectUnauthorized: false,
        });
        if(requestId !== this.placementInventoryRequestId) {
          return;
        }
        if(!inventoryResponse || !inventoryResponse.inventory || !Array.isArray(inventoryResponse.inventory.nodes)) {
          throw new Error('Inventory response was incomplete');
        }
        this.placementInventory = inventoryResponse.inventory.nodes;
        this.placementInventoryInsecureTLS = inventoryResponse.insecureTLS === true;
        this.resourcePools = inventoryResponse.inventory.resourcePools || [];
        this.currentValue.placementClusterId = inventoryResponse.placement?.clusterId || '';
        this.currentValue.placementLedgerName = inventoryResponse.placement?.ledgerName || '';
        this.currentValue.placementLedgerNamespace = inventoryResponse.placement?.ledgerNamespace || 'fleet-default';
      } catch(error) {
        if(requestId === this.placementInventoryRequestId) {
          const status = error?._status ?? error?.status ?? error?.response?.status;
          if(status === 403 && error?.error === 'proxmox_host_not_allowed' && error?.host) {
            this.placementInventoryBlockedHost = error.host;
            this.placementInventoryError = `Proxmox host ${error.host} is not approved for this node driver.`;
          } else if(status === 401 || status === 403) {
            this.placementInventoryError = 'You are not authorized to use this cloud credential for inventory. Ask a Rancher administrator to grant access to the selected credential.';
          } else if(status === 404) {
            this.placementInventoryError = 'The placement inventory API is not available. Ask a Rancher administrator to check the inventory controller.';
          } else if(status === 502) {
            this.placementInventoryError = 'Proxmox inventory could not be read. Check the credential and Proxmox API access, then retry.';
          } else {
            this.placementInventoryError = status
              ? `Inventory discovery failed (HTTP ${status}). Check Rancher access and the inventory controller, then retry.`
              : 'Inventory request failed before receiving an HTTP response. Check the local preview connection to Rancher, then retry.';
          }
        }
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async refreshPlacementInventory() {
      await this.fetchPlacementInventory();
    },
    async addInventoryHostToWhitelist() {
      if(!this.placementInventoryBlockedHost || this.placementInventoryAllowlistBusy) {
        return;
      }
      this.placementInventoryAllowlistBusy = true;
      try {
        const nodeDriver = await this.$store.dispatch('rancher/find', { type: 'nodedriver', id: 'pve' });
        const whitelist = nodeDriver.whitelistDomains || [];
        if(!whitelist.includes(this.placementInventoryBlockedHost)) {
          nodeDriver.whitelistDomains = [...whitelist, this.placementInventoryBlockedHost];
          await nodeDriver.save();
        }
        await this.fetchPlacementInventory();
      } catch(_error) {
        this.placementInventoryError = 'Could not update the Proxmox host allowlist. Ask a Rancher administrator to approve this host.';
      } finally {
        this.placementInventoryAllowlistBusy = false;
      }
    },
    togglePlacementHost(host, checked) {
      this.selectedPlacementHosts = checked
        ? [...new Set([...this.selectedPlacementHosts, host])]
        : this.selectedPlacementHosts.filter(selected => selected !== host);
    },
    togglePlacementStorage(storage, checked) {
      this.selectedPlacementStorages = checked
        ? [...new Set([...this.selectedPlacementStorages, storage])]
        : this.selectedPlacementStorages.filter(selected => selected !== storage);
    },
    setMemoryBallooningEnabled(enabled) {
      this.memoryBallooningEnabled = enabled;
      this.currentValue.memoryBalloon = enabled ? (this.currentValue.memory || '') : 0;
    },
    setPlacementTemplate(host, vmid) {
      if(vmid) {
        this.placementTemplateSelections = { ...this.placementTemplateSelections, [host]: vmid };
      } else {
        const selections = { ...this.placementTemplateSelections };
        delete selections[host];
        this.placementTemplateSelections = selections;
      }
      this.syncPlacementTemplateMap();
    },
    syncPlacementTemplateMap() {
      this.currentValue.placementTemplates = this.selectedPlacementHosts
        .filter(host => this.placementTemplateSelections[host])
        .map(host => `${host}=${this.placementTemplateSelections[host]}`)
        .join(',');
      this.currentValue.placementGuestReady = false;
    },
    placementTemplatesForHost(host) {
      const node = (this.placementInventory || []).find(item => item.name === host);
      const templates = node && node.templates ? [...node.templates] : [];
      const selected = this.placementTemplateSelections[host];
      if(selected && !templates.some(template => String(template.vmid) === String(selected))) {
        templates.push({ vmid: selected, name: 'Previously configured' });
      }
      return templates;
    },
    selectedPlacementTemplate(host) {
      const selected = this.placementTemplateSelections[host];
      return this.placementTemplatesForHost(host).find(item => String(item.vmid) === String(selected));
    },
    placementTemplateCompatible(host) {
      const template = this.selectedPlacementTemplate(host);
      return !!template && Array.isArray(template.compatibilityReasons) && template.compatibilityReasons.length === 0;
    },
    fetchFromProxmox(apiPath){
      if(this.credential == null) {
        throw new Error("Credential not available")
      }

      return this.$store.dispatch('management/request', {
        method: 'GET',
        url: '/meta/proxy/' + this.credential.url.replace(/^https?:\/\//, '') + apiPath,
        headers: {
          "X-API-Auth-Header": `PVEAPIToken=${this.credential.tokenId}=${this.credential.tokenSecret}`,
        },
        redirectUnauthorized: false,
      })
    },
    selectTemplate(option) {
      if(Object.keys(this.templateSelectOptions).length == 0) {
        this.currentValue.template = 0;
        return;
      }

      for (const [vmid, name] of Object.entries(this.templateSelectOptions)) {
        if(option == name) {
          this.currentValue.template = vmid;
          return;
        }
      }

      this.currentValue.template = 0;
    },
  },
  computed: {
    disabled(){
      return this.fetchingCount > 0
    },
    templateSelectOptions() {
      if(this.templates === null) {
        return {};
      }

      return this.templates.reduce(
        (templates, template) => ({...templates, [template.vmid]: `${template.name} (${template.vmid})`}),
        {},
      );
    },
    templateSelectValue() {
      return this.templateSelectOptions[this.currentValue.template] ?? '';
    },
    isoDeviceSelectOptions() {
      if(this.devices == null) {
        return null;
      }

      return this.devices
        .filter(device => device.key)
        .filter(device => (device.value && typeof device.value == "string"))
        .filter(device => device.value.includes('media=cdrom'))
        .map(device => device.key);
    },
    networkInterfaceSelectOptions() {
      if(this.devices == null) {
        return null;
      }

      return this.devices
        .filter(device => device.key)
        .filter(device => device.key.startsWith('net'))
        .map(device => device.key);
    },
    placementStorageChoices() {
      if(!this.placementInventory || this.selectedPlacementHosts.length === 0) {
        return [...new Set(this.selectedPlacementStorages)].sort();
      }
      const selectedNodes = this.selectedPlacementHosts.map(host => this.placementInventory.find(node => node.name === host));
      if(selectedNodes.some(node => !node || !node.online || node.storages == null)) {
        return [...new Set(this.selectedPlacementStorages)].sort();
      }
      const eligible = node => (node.storages || []).filter(storage => storage.ready && storage.images && !storage.shared).map(storage => storage.id);
      const common = selectedNodes.map(eligible).reduce((left, right) => left.filter(storage => right.includes(storage)));
      return [...new Set([...this.selectedPlacementStorages, ...common])].sort();
    },
    placementHostChoices() {
      const discovered = (this.placementInventory || []).map(node => node.name);
      return [...new Set([...this.selectedPlacementHosts, ...discovered])].sort();
    },
    placementTemplatesValid() {
      const entries = (this.currentValue.placementTemplates || '').split(',').filter(Boolean);
      if(entries.length !== this.selectedPlacementHosts.length) {
        return false;
      }
      const mappings = new Map();
      for(const entry of entries) {
        const [host, vmid, extra] = entry.split('=');
        if(!host || !vmid || extra !== undefined || !/^\d+$/.test(vmid) || Number(vmid) < 100 || Number(vmid) > 999999999 || mappings.has(host)) {
          return false;
        }
        mappings.set(host, vmid);
      }
      return this.selectedPlacementHosts.every(host => mappings.has(host)) && new Set(mappings.values()).size === mappings.size;
    },
  },
}
</script>

<template>
  <div class="mt-20 mb-20">
    <h3>
      <t k="cluster.machineConfig.pve.template.header" />
    </h3>
    <div class="row mb-10">
      <div class="col" :class="currentValue.placementEnabled ? 'span-12' : 'span-6'">
        <!-- Resource pool  -->
        <LabeledInput
          v-if="!currentValue.placementEnabled && resourcePools == null"
          type="text"
          :mode="mode"
          :disabled="disabled"
          :value="currentValue.resourcePool"
          @change="e => { currentValue.resourcePool = e.target.value}"
          label-key="cluster.machineConfig.pve.template.resourcePool.label"
          required
        />

        <LabeledSelect
          v-else-if="resourcePools != null"
          :mode="mode"
          :disabled="disabled"
          v-model:value="currentValue.resourcePool"
          :options="resourcePools"
          label-key="cluster.machineConfig.pve.template.resourcePool.label"
          required
        />
        <p v-if="currentValue.placementEnabled && resourcePools == null" class="text-muted">
          Proxmox resource pools will be selectable after inventory discovery succeeds.
        </p>
      </div>
      <div class="col span-6">
        <!-- Template -->
        <LabeledInput
          v-if="!currentValue.placementEnabled && templates == null"
          type="number"
          :mode="mode"
          :disabled="disabled"
          :value="currentValue.template"
          @change="e => { currentValue.template = e.target.value}"
          label-key="cluster.machineConfig.pve.template.templateID.label"
          required
          min="1"
          step="1"
        />

        <LabeledSelect
          v-else-if="!currentValue.placementEnabled"
          :mode="mode"
          :disabled="disabled || (resourcePools != null && !currentValue.resourcePool)"
          v-model:value="templateSelectValue"
          @option:selected="selectTemplate"
          :options="Object.values(templateSelectOptions)"
          label-key="cluster.machineConfig.pve.template.templateID.label"
          required
        />
      </div>
    </div>
    <div v-if="!currentValue.placementEnabled" class="row mb-20">
      <div class="col span-6">
        <!-- ISO Device -->
        <LabeledInput
          v-if="!currentValue.placementEnabled && devices == null"
          type="text"
          :mode="mode"
          :disabled="disabled"
          :value="currentValue.isoDevice"
          @change="e => { currentValue.isoDevice = e.target.value}"
          label-key="cluster.machineConfig.pve.template.iso.label"
          tooltip-key="cluster.machineConfig.pve.template.iso.tooltip"
          required
        />

        <LabeledSelect
          v-else-if="!currentValue.placementEnabled"
          :mode="mode"
          :disabled="disabled || (templates != null && !currentValue.template)"
          v-model:value="currentValue.isoDevice"
          :options="isoDeviceSelectOptions"
          label-key="cluster.machineConfig.pve.template.iso.label"
          tooltip-key="cluster.machineConfig.pve.template.iso.tooltip"
          required
        />
      </div>
      <div class="col span-6">
        <!-- Network interface -->
        <LabeledInput
          v-if="!currentValue.placementEnabled && devices == null"
          type="text"
          :mode="mode"
          :value="currentValue.networkInterface"
          @change="e => { currentValue.networkInterface = e.target.value}"
          label-key="cluster.machineConfig.pve.template.network.label"
          tooltip-key="cluster.machineConfig.pve.template.network.tooltip"
          required
        />

        <LabeledSelect
          v-else-if="!currentValue.placementEnabled"
          :mode="mode"
          :disabled="disabled || (templates != null && !currentValue.template)"
          v-model:value="currentValue.networkInterface"
          :options="networkInterfaceSelectOptions"
          label-key="cluster.machineConfig.pve.template.network.label"
          tooltip-key="cluster.machineConfig.pve.template.network.tooltip"
          required
        />
        <p v-else class="text-muted">Placement infers net0, or the sole template interface, and preserves template networking.</p>
      </div>
    </div>

    <h3>
      <t k="cluster.machineConfig.pve.hardware.header" />
    </h3>
    <div class="row mb-20">
      <div class="col span-6">
        <!-- Processor cores -->
        <UnitInput
          type="number"
          :mode="mode"
          :disabled="disabled || (templates != null && !currentValue.template)"
          v-model:value="currentValue.processorCores"
          label-key="cluster.machineConfig.pve.hardware.processorCores.label"
          suffix="cores"
          min="0"
          step="1"
        />
      </div>
      <div class="col span-6">
        <!-- Memory -->
        <div class="memory-ballooning-control">
          <UnitInput
            type="number"
            :mode="mode"
            :disabled="disabled || (templates != null && !currentValue.template)"
            v-model:value="currentValue.memory"
            label-key="cluster.machineConfig.pve.hardware.memory.label"
            suffix="MiB"
            min="0"
            step="256"
          />
          <Checkbox
            :value="memoryBallooningEnabled"
            :disabled="disabled || (!currentValue.placementEnabled && templates != null && !currentValue.template)"
            label="Enable memory ballooning"
            @update:value="setMemoryBallooningEnabled"
          />
        </div>
      </div>
    </div>
    <div v-if="currentValue.placementEnabled" class="row mb-20">
      <div class="col span-6">
        <LabeledInput
          type="number"
          min="1"
          step="1"
          :mode="mode"
          v-model:value="currentValue.diskSizeGiB"
          label="Boot disk size (GiB)"
          required
        />
      </div>
      <div class="col span-6">
        <h4 class="mt-0"><t k="cluster.machineConfig.pve.template.cloning.header" /></h4>
        <Checkbox
          :mode="mode"
          v-model:value="currentValue.fullClone"
          label-key="cluster.machineConfig.pve.template.cloning.label"
          tooltip-key="cluster.machineConfig.pve.template.cloning.tooltip"
        />
      </div>
    </div>

      <h3><t k="cluster.machineConfig.pve.placement.header" /></h3>
      <div class="row mb-10">
        <div class="col span-12">
          <Checkbox v-model:value="currentValue.placementEnabled" label="Enable safe Proxmox placement" />
          <p class="text-muted">When enabled, host-local templates and the management-cluster placement ledger are required. The legacy single-template/ISO flow is not used.</p>
        </div>
      </div>
      <template v-if="currentValue.placementEnabled">
        <div class="placement-panel">
          <div class="placement-panel-heading">
            <div><h4>Proxmox inventory</h4><p class="text-muted">Inventory loads automatically from the selected cloud credential.</p></div>
            <span class="placement-status" role="status">{{ fetchingCount > 0 ? 'Discovering…' : placementInventory ? `${placementInventory.length} hosts discovered` : 'Not available' }}</span>
          </div>
            <p v-if="placementInventoryInsecureTLS" class="text-warning">TLS certificate verification is disabled for this credential. Proxmox API traffic may be intercepted.</p>
            <button v-if="placementInventoryBlockedHost" type="button" class="btn role-secondary ml-10" :disabled="disabled || placementInventoryAllowlistBusy" @click="addInventoryHostToWhitelist">{{ placementInventoryAllowlistBusy ? 'Adding host…' : 'Add host to allowlist and retry' }}</button>
            <button v-else-if="placementInventoryError" type="button" class="btn role-secondary ml-10" :disabled="disabled" @click="refreshPlacementInventory">Retry discovery</button>
            <p v-if="placementInventoryError" class="text-error">{{ placementInventoryError }}</p>
        </div>
        <section class="placement-section">
        <h4>Hosts and storage</h4>
        <p class="text-muted">Choose hosts and image storage available locally on every selected host.</p>
        <div class="row mb-10">
          <div class="col span-6">
            <label>Allowed Proxmox hosts</label>
            <div v-if="placementHostChoices.length" class="placement-choice-list">
              <label v-for="host in placementHostChoices" :key="host" class="placement-choice">
                <input type="checkbox" :disabled="disabled" :checked="selectedPlacementHosts.includes(host)" @change="togglePlacementHost(host, $event.target.checked)" />
                {{ host }}{{ placementInventory && placementInventory.find(node => node.name === host && !node.online) ? ' (offline)' : '' }}
              </label>
            </div>
            <p v-else class="text-muted">No hosts discovered yet. Select a Proxmox credential to load inventory automatically.</p>
          </div>
          <div class="col span-6">
            <label>Allowed host-local storages</label>
            <div v-if="placementStorageChoices.length" class="placement-choice-list">
              <label v-for="storage in placementStorageChoices" :key="storage" class="placement-choice">
                <input type="checkbox" :disabled="disabled" :checked="selectedPlacementStorages.includes(storage)" @change="togglePlacementStorage(storage, $event.target.checked)" />
                {{ storage }}
              </label>
            </div>
            <p v-else class="text-muted">Select allowed hosts to show their common image storages.</p>
          </div>
        </div>
        </section>
        <section class="placement-section">
        <h4>Host-local templates</h4>
        <p class="text-muted">Choose a compatible template on each selected host. Template names do not imply equivalence.</p>
        <div class="row mb-10">
          <div class="col span-12">
            <div v-for="host in selectedPlacementHosts" :key="host" class="placement-template-card">
              <label :for="`placement-template-${uuid}-${host}`" class="placement-host-name">{{ host }}</label>
              <div>
                <select :id="`placement-template-${uuid}-${host}`" class="form-control" :disabled="disabled" :value="placementTemplateSelections[host] || ''" @change="setPlacementTemplate(host, $event.target.value)" required>
                  <option value="">Select a local template</option>
                  <option v-for="template in placementTemplatesForHost(host)" :key="template.vmid" :value="String(template.vmid)">
                    {{ template.name || 'unnamed' }} ({{ template.vmid }}){{ template.compatibilityReasons && template.compatibilityReasons.length ? ' — incompatible' : '' }}
                  </option>
                </select>
                 <div v-if="placementTemplateSelections[host]" class="placement-template-details text-muted mt-5">
                  <template v-if="selectedPlacementTemplate(host)">
                    <div>Boot disk: {{ selectedPlacementTemplate(host).bootDisk || 'unverified' }}</div>
                    <div>Network: {{ selectedPlacementTemplate(host).networkInterface || 'ambiguous' }} on {{ selectedPlacementTemplate(host).networkBridge || 'unavailable bridge' }}; dynamic cloud-init address: {{ selectedPlacementTemplate(host).dynamicAddress ? 'verified' : 'not verified' }}</div>
                    <div>Cloud-init drive: {{ selectedPlacementTemplate(host).cloudInit ? 'present' : 'missing' }}; guest agent: {{ selectedPlacementTemplate(host).guestAgent ? 'present' : 'missing' }}</div>
                    <div>Template disks: {{ Object.entries(selectedPlacementTemplate(host).disks || {}).map(([device, config]) => `${device}=${config}`).join('; ') || 'unavailable' }}</div>
                    <div>Disk storage: {{ Object.entries(selectedPlacementTemplate(host).diskStorages || {}).map(([device, storage]) => `${device}=${storage}`).join(', ') || 'unverified' }}</div>
                    <div v-if="selectedPlacementTemplate(host).compatibilityReasons && selectedPlacementTemplate(host).compatibilityReasons.length" class="text-error">{{ selectedPlacementTemplate(host).compatibilityReasons.join('; ') }}</div>
                  </template>
                  <div v-else class="text-warning">Previously selected template is unavailable in discovered inventory.</div>
                </div>
              </div>
            </div>
            <p v-if="!selectedPlacementHosts.length" class="text-muted">Select hosts above to choose their templates.</p>
            <p v-if="!placementInventory" class="text-muted">No template discovery data is available. Existing mappings are preserved; configure host-local templates after discovery succeeds.</p>
          </div>
        </div>
        </section>
        <section class="placement-section">
        <h4>Guest readiness</h4>
        <div class="row mb-10">
          <div class="col span-12">
            <label :for="`cloud-init-network-${uuid}`">Custom cloud-init network snippet (optional)</label>
            <input :id="`cloud-init-network-${uuid}`" v-model="currentValue.cloudInitNetworkSnippet" class="form-control" :disabled="disabled" placeholder="local-vm1:snippets/dhcp-network.yaml" />
            <p class="text-muted">Provide an existing snippet on every selected host. It replaces Proxmox-generated network configuration; verify its interfaces and DHCP DNS settings match the template.</p>
          </div>
        </div>
        <div class="row mb-10">
          <div class="col span-12">
            <Checkbox v-model:value="currentValue.placementGuestReady" label="I verified every selected guest template supports SSH and passwordless sudo for the configured user." />
            <p class="text-muted">Changing selected hosts or templates, or refreshing inventory, clears this confirmation. Older placement configurations require this review before use.</p>
          </div>
        </div>
        </section>
        <details class="placement-section">
        <summary>Advanced placement coordination</summary>
        <p class="text-muted">Placement coordination is automatically scoped to the selected Proxmox API endpoint. Each distinct endpoint uses its own ledger; credential token rotation does not change its identity.</p>
        </details>
        <details v-if="placementInventory" class="placement-inventory placement-section">
          <summary>Show discovered inventory details</summary>
          <ul>
            <li v-for="node in placementInventory" :key="node.name">
              {{ node.name }} — {{ node.online ? 'online' : 'offline' }}; image storages: {{ node.storages ? (node.storages.filter(storage => storage.images).map(storage => `${storage.id}${storage.shared ? ' (shared)' : ''}`).join(', ') || 'none' ) : 'unavailable' }};
              templates: {{ node.templates ? (node.templates.map(template => `${template.name || 'unnamed'}=${template.vmid}`).join(', ') || 'none') : 'unavailable' }}{{ node.error ? ` — ${node.error}` : '' }}
            </li>
          </ul>
        </details>
      </template>

    <div>
      <h3>
        <t k="cluster.machineConfig.pve.vmTags.header" />
      </h3>
      <div class="row mb-20">
        <div class="col span-12">
          <!-- VM Tags -->
          <LabeledInput
            type="text"
            :mode="mode"
            v-model:value="currentValue.tags"
            label-key="cluster.machineConfig.pve.vmTags.label"
            tooltip-key="cluster.machineConfig.pve.vmTags.tooltip"
            placeholder="tag1,tag2"
          />
        </div>
      </div>

      <h3>
        <t k="cluster.machineConfig.pve.ssh.header" />
      </h3>
      <div class="row mb-20">
        <div class="col span-6">
          <!-- SSH Username -->
          <LabeledInput
            type="text"
            :mode="mode"
            v-model:value="currentValue.sshUser"
            label-key="cluster.machineConfig.pve.ssh.username.label"
            tooltip-key="cluster.machineConfig.pve.ssh.username.tooltip"
            required
          />
        </div>
        <div class="col span-6">
          <!-- SSH Port -->
          <LabeledInput
            type="number"
            :mode="mode"
            v-model:value="currentValue.sshPort"
            label-key="cluster.machineConfig.pve.ssh.port.label"
            tooltip-key="cluster.machineConfig.pve.ssh.port.tooltip"
            required
            min="1"
            step="1"
          />
        </div>
      </div>

    </div>
  </div>
</template>

<style scoped>
.placement-panel, .placement-section {
  margin: 20px 0;
  padding: 18px 20px;
  border: 1px solid var(--border, #d5d8dc);
  border-radius: 6px;
}

.placement-panel-heading {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px 16px;
}

.placement-panel h4, .placement-section h4 { margin: 0 0 6px; }
.placement-panel p, .placement-section > p { margin: 0 0 14px; }
.placement-status { padding: 4px 10px; border-radius: 12px; background: var(--sortable-table-header-bg, #edf0f2); font-size: 0.9em; }
.placement-optional { font-size: 0.85em; font-weight: normal; }
.placement-template-card { padding: 14px; margin: 10px 0; border: 1px solid var(--border, #d5d8dc); border-radius: 4px; }
.placement-host-name { display: block; margin-bottom: 8px; font-weight: 600; }
.placement-template-details { line-height: 1.6; overflow-wrap: anywhere; }
.placement-inventory summary { cursor: pointer; font-weight: 600; }
.placement-inventory ul { margin: 12px 0 0; padding-left: 20px; }
.placement-inventory li { margin-bottom: 8px; overflow-wrap: anywhere; }
.memory-ballooning-control { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; }
.memory-ballooning-control > :first-child { flex: 1 1 240px; min-width: 0; }
.placement-choice-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  max-height: 180px;
  overflow-y: auto;
  padding: 8px 10px;
  border: 1px solid var(--input-border, #ccc);
  border-radius: 4px;
}

.placement-choice {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-weight: normal;
}
</style>
