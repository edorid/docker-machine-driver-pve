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
      selectedPlacementHosts: (this.value.placementHosts || '').split(',').map(value => value.trim()).filter(Boolean),
      selectedPlacementStorages: (this.value.placementStorages || '').split(',').map(value => value.trim()).filter(Boolean),
      placementTemplateSelections: (this.value.placementTemplates || '').split(',').reduce((result, entry) => {
        const [host, vmid] = entry.split('=');
        if(host && vmid) {
          result[host.trim()] = vmid.trim();
        }
        return result;
      }, {}),

      // Current input values.
      currentValue: {
        resourcePool: this.value.resourcePool ?? '',
        template: this.value.template ? parseInt(this.value.template) : 0,
        isoDevice: this.value.isoDevice ?? '',
        networkInterface: this.value.networkInterface ?? '',
        sshUser: this.value.sshUser ? this.value.sshUser : 'service',
        sshPort: this.value.sshPort ? parseInt(this.value.sshPort) : 22,
        processorSockets: this.value.processorSockets ? parseInt(this.value.processorSockets) : "",
        processorCores: this.value.processorCores ? parseInt(this.value.processorCores) : "",
        memory: this.value.memory ? parseInt(this.value.memory) : "",
        memoryBalloon: this.value.memoryBalloon ? parseInt(this.value.memoryBalloon) : "",
        fullClone: this.value.fullClone ?? false,
        tags: this.value.tags ?? '',
        placementEnabled: this.value.placementGroup != null && this.value.placementGroup !== '',
        placementGroup: this.value.placementGroup ?? '',
        placementHosts: this.value.placementHosts ?? '',
        placementStorages: this.value.placementStorages ?? '',
        placementTemplates: this.value.placementTemplates ?? '',
        diskSizeGiB: this.value.diskSizeGiB ?? '',
        placementLedgerNamespace: this.value.placementLedgerNamespace ?? '',
        placementLedgerName: this.value.placementLedgerName ?? '',
        placementClusterId: this.value.placementClusterId ?? '',
        rancherClusterName: this.value.rancherClusterName ?? '',
        rancherMachinePool: this.value.rancherMachinePool ?? '',
      },
    }
  },
  created(){
    this.validate();
  },
  async fetch(){
    await this.fetchCredential();
    await this.fetchResourcePools();
    await this.fetchTemplates();
    await this.fetchDevices();
    await this.fetchPlacementInventory();
  },
  watch: {
    async credentialId(){
      await this.$fetch();
      this.validate();
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
      if(this.currentValue.memoryBalloon == 0) {
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

      if(this.currentValue.networkInterface == '') {
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

      if(this.currentValue.memoryBalloon != "" && this.currentValue.memoryBalloon < 0) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.memory != "" && this.currentValue.memoryBalloon != "" && this.currentValue.memory < this.currentValue.memoryBalloon) {
        this.$emit('validationChanged', false);
        return
      }

      if(this.currentValue.placementEnabled && (
        !this.currentValue.placementGroup.trim() || !this.currentValue.placementHosts.trim() ||
        !this.currentValue.placementStorages.trim() || !this.currentValue.placementTemplates.trim() ||
        !this.placementTemplatesValid ||
        !this.currentValue.diskSizeGiB || Number(this.currentValue.diskSizeGiB) < 1 ||
        !this.currentValue.placementLedgerNamespace.trim() || !this.currentValue.placementLedgerName.trim() ||
        !this.currentValue.placementClusterId.trim() || this.currentValue.processorSockets < 1 ||
        this.currentValue.processorCores < 1 || this.currentValue.memory < 1
      )) {
        this.$emit('validationChanged', false);
        return
      }

      // Copy current value to the 'value' prop
      this.value.resourcePool = this.currentValue.resourcePool;
      this.value.template = this.currentValue.placementEnabled ? '' : this.currentValue.template.toString();
      this.value.isoDevice = this.currentValue.placementEnabled ? '' : this.currentValue.isoDevice;
      this.value.networkInterface = this.currentValue.networkInterface;
      this.value.sshUser = this.currentValue.sshUser;
      this.value.sshPort = this.currentValue.sshPort.toString();
      this.value.processorSockets = this.currentValue.processorSockets.toString();
      this.value.processorCores = this.currentValue.processorCores.toString();
      this.value.memory = this.currentValue.memory.toString();
      this.value.memoryBalloon = this.currentValue.memoryBalloon.toString();
      this.value.fullClone = this.currentValue.fullClone;
      this.value.placementGroup = this.currentValue.placementEnabled ? this.currentValue.placementGroup.trim() : '';
      this.value.placementHosts = this.currentValue.placementEnabled ? this.currentValue.placementHosts.trim() : '';
      this.value.placementStorages = this.currentValue.placementEnabled ? this.currentValue.placementStorages.trim() : '';
      this.value.placementTemplates = this.currentValue.placementEnabled ? this.currentValue.placementTemplates.trim() : '';
      this.value.diskSizeGiB = this.currentValue.placementEnabled ? this.currentValue.diskSizeGiB.toString() : '';
      this.value.placementLedgerNamespace = this.currentValue.placementEnabled ? this.currentValue.placementLedgerNamespace.trim() : '';
      this.value.placementLedgerName = this.currentValue.placementEnabled ? this.currentValue.placementLedgerName.trim() : '';
      this.value.placementClusterId = this.currentValue.placementEnabled ? this.currentValue.placementClusterId.trim() : '';
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
      this.placementInventoryError = '';
      if(this.credential == null) {
        this.placementInventory = null;
        this.placementInventoryError = 'Select a Proxmox credential with a trusted TLS certificate to discover inventory.';
        return;
      }
      try {
        this.fetchingCount += 1;
        const { data: nodes } = await this.fetchFromProxmox('/api2/json/nodes');
        const inventory = await Promise.all(nodes.map(async node => {
          const result = { name: node.node, online: node.status === 'online', templates: null, storages: null, error: '' };
          if(!result.online) {
            return result;
          }
          try {
            const [vmResponse, storageResponse] = await Promise.all([
              this.fetchFromProxmox(`/api2/json/nodes/${encodeURIComponent(node.node)}/qemu`),
              this.fetchFromProxmox(`/api2/json/nodes/${encodeURIComponent(node.node)}/storage`),
            ]);
            result.templates = vmResponse.data.filter(vm => vm.template == 1).map(vm => ({ vmid: vm.vmid, name: vm.name }));
            result.storages = storageResponse.data.filter(storage => storage.enabled == 1 && storage.active == 1 && (storage.content || '').split(',').includes('images')).map(storage => storage.storage);
          } catch(e) {
            // Keep the node visible but don't imply that inaccessible data is empty.
            result.error = e?.message || 'Unable to read host inventory.';
          }
          return result;
        }));
        this.placementInventory = inventory;
      } catch(e) {
        // Discovery is advisory only; the driver performs authoritative, fail-closed validation.
        this.placementInventory = null;
        this.placementInventoryError = e?.message || 'The Proxmox inventory request failed.';
      } finally {
        this.fetchingCount -= 1;
      }
    },
    async refreshPlacementInventory() {
      await this.fetchPlacementInventory();
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
      const common = selectedNodes.map(node => node.storages).reduce((left, right) => left.filter(storage => right.includes(storage)));
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
      <div class="col span-6">
        <!-- Resource pool  -->
        <LabeledInput
          v-if="resourcePools == null"
          type="text"
          :mode="mode"
          :disabled="disabled"
          :value="currentValue.resourcePool"
          @change="e => { currentValue.resourcePool = e.target.value}"
          label-key="cluster.machineConfig.pve.template.resourcePool.label"
          required
        />

        <LabeledSelect
          v-else
          :mode="mode"
          :disabled="disabled"
          v-model:value="currentValue.resourcePool"
          :options="resourcePools"
          label-key="cluster.machineConfig.pve.template.resourcePool.label"
          required
        />
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
    <div class="row mb-20">
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
          v-if="devices == null"
          type="text"
          :mode="mode"
          :value="currentValue.networkInterface"
          @change="e => { currentValue.networkInterface = e.target.value}"
          label-key="cluster.machineConfig.pve.template.network.label"
          tooltip-key="cluster.machineConfig.pve.template.network.tooltip"
          required
        />

        <LabeledSelect
          v-else
          :mode="mode"
          :disabled="disabled || (templates != null && !currentValue.template)"
          v-model:value="currentValue.networkInterface"
          :options="networkInterfaceSelectOptions"
          label-key="cluster.machineConfig.pve.template.network.label"
          tooltip-key="cluster.machineConfig.pve.template.network.tooltip"
          required
        />
      </div>
    </div>

    <h3>
      <t k="cluster.machineConfig.pve.hardware.header" />
    </h3>
    <div class="row mb-10">
      <div class="col span-6">
        <!-- Processor sockets -->
        <UnitInput
          type="number"
          :mode="mode"
          :disabled="disabled || (templates != null && !currentValue.template)"
          v-model:value="currentValue.processorSockets"
          label-key="cluster.machineConfig.pve.hardware.processorSockets.label"
          suffix="sockets"
          min="0"
          step="1"
        />
      </div>
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
    </div>

    <div class="row mb-20">
      <div class="col span-6">
        <!-- Memory -->
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
      </div>
    </div>

    <portal :to="`advanced-${uuid}`">
      <h3><t k="cluster.machineConfig.pve.placement.header" /></h3>
      <div class="row mb-10">
        <div class="col span-12">
          <Checkbox v-model:value="currentValue.placementEnabled" label="Enable safe Proxmox placement" />
          <p class="text-muted">When enabled, host-local templates and the management-cluster placement ledger are required. The legacy single-template/ISO flow is not used.</p>
        </div>
      </div>
      <template v-if="currentValue.placementEnabled">
        <div class="row mb-10">
          <div class="col span-12">
            <strong>Proxmox inventory</strong>
            <span v-if="fetchingCount > 0" class="text-muted"> — discovering…</span>
            <span v-else-if="placementInventory" class="text-muted"> — {{ placementInventory.length }} host(s) discovered</span>
            <button type="button" class="btn role-secondary ml-10" :disabled="disabled" @click="refreshPlacementInventory">{{ placementInventory ? 'Refresh' : 'Discover hosts' }}</button>
            <p v-if="placementInventoryError" class="text-muted">{{ placementInventoryError }}</p>
          </div>
        </div>
        <div class="row mb-10">
          <div class="col span-6">
            <LabeledInput type="text" :mode="mode" v-model:value="currentValue.placementGroup" label="Placement group identity" required />
          </div>
          <div class="col span-6">
            <LabeledInput type="number" min="1" step="1" :mode="mode" v-model:value="currentValue.diskSizeGiB" label="Boot disk size (GiB)" required />
          </div>
        </div>
        <div class="row mb-10">
          <div class="col span-6">
            <label>Allowed Proxmox hosts</label>
            <div v-if="placementHostChoices.length" class="placement-choice-list">
              <label v-for="host in placementHostChoices" :key="host" class="placement-choice">
                <input type="checkbox" :disabled="disabled" :checked="selectedPlacementHosts.includes(host)" @change="togglePlacementHost(host, $event.target.checked)" />
                {{ host }}{{ placementInventory && placementInventory.find(node => node.name === host && !node.online) ? ' (offline)' : '' }}
              </label>
            </div>
            <p v-else class="text-muted">No hosts discovered yet. Choose the Proxmox credential above and select Discover hosts.</p>
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
        <div class="row mb-10">
          <div class="col span-12">
            <label>Host-local template for each allowed host</label>
            <div v-for="host in selectedPlacementHosts" :key="host" class="row mb-10">
              <div class="col span-4">{{ host }}</div>
              <div class="col span-8">
                <select class="form-control" :disabled="disabled" :value="placementTemplateSelections[host] || ''" @change="setPlacementTemplate(host, $event.target.value)" required>
                  <option value="">Select a local template</option>
                  <option v-for="template in placementTemplatesForHost(host)" :key="template.vmid" :value="String(template.vmid)">
                    {{ template.name || 'unnamed' }} ({{ template.vmid }})
                  </option>
                </select>
              </div>
            </div>
            <p v-if="!placementInventory" class="text-muted">No template discovery data is available. Existing mappings are preserved; configure host-local templates after discovery succeeds.</p>
          </div>
        </div>
        <div class="row mb-10">
          <div class="col span-4"><LabeledInput type="text" :mode="mode" v-model:value="currentValue.placementLedgerNamespace" label="Ledger namespace" required /></div>
          <div class="col span-4"><LabeledInput type="text" :mode="mode" v-model:value="currentValue.placementLedgerName" label="Ledger name" required /></div>
          <div class="col span-4"><LabeledInput type="text" :mode="mode" v-model:value="currentValue.placementClusterId" label="Proxmox cluster identity" required /></div>
        </div>
        <div class="row mb-10">
          <div class="col span-6"><LabeledInput type="text" :mode="mode" v-model:value="currentValue.rancherClusterName" label="Rancher cluster name for VM tags (optional)" /></div>
          <div class="col span-6"><LabeledInput type="text" :mode="mode" v-model:value="currentValue.rancherMachinePool" label="Rancher machine pool for VM tags (optional)" /></div>
        </div>
        <p class="text-muted">When supplied, the driver adds normalized <code>rancher-cluster-&lt;name&gt;</code> and <code>rancher-pool-&lt;name&gt;</code> tags automatically. Rancher does not pass these names to the driver, so enter the desired labels here.</p>
        <p class="text-muted">Enter only hosts, storages, and template IDs verified for this Proxmox cluster. The driver revalidates inventory at create time and fails closed on incomplete API access.</p>
        <div v-if="placementInventory" class="placement-inventory">
          <strong>Discovered Proxmox inventory (advisory)</strong>
          <ul>
            <li v-for="node in placementInventory" :key="node.name">
              {{ node.name }} — {{ node.online ? 'online' : 'offline' }}; image storages: {{ node.storages ? (node.storages.join(', ') || 'none') : 'unavailable' }};
              templates: {{ node.templates ? (node.templates.map(template => `${template.name || 'unnamed'}=${template.vmid}`).join(', ') || 'none') : 'unavailable' }}{{ node.error ? ` — ${node.error}` : '' }}
            </li>
          </ul>
        </div>
        <div v-else class="text-muted">
          <p>Proxmox host/template discovery unavailable. Configure these values manually; provisioning will fail closed if they are invalid.</p>
        </div>
      </template>

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
        <t k="cluster.machineConfig.pve.memoryBalloon.header" />
      </h3>
      <div class="row mb-20">
        <div class="col span-6">
          <!-- Memory balloon -->
          <UnitInput
            type="number"
            :mode="mode"
            :disabled="disabled || (templates != null && !currentValue.template)"
            v-model:value="currentValue.memoryBalloon"
            label-key="cluster.machineConfig.pve.memoryBalloon.minimumMemory.label"
            tooltip-key="cluster.machineConfig.pve.memoryBalloon.minimumMemory.tooltip"
            suffix="MiB"
            min="0"
            step="256"
            :max="currentValue.memory != '' ? currentValue.memory : undefined"
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

      <h3>
        <t k="cluster.machineConfig.pve.template.header" />
      </h3>
      <div class="row">
        <div class="col span-6">
          <!-- Force full clone -->
          <Checkbox
            :mode="mode"
            v-model:value="currentValue.fullClone"
            label-key="cluster.machineConfig.pve.template.cloning.label"
            tooltip-key="cluster.machineConfig.pve.template.cloning.tooltip"
          />
        </div>
      </div>
    </portal>
  </div>
</template>

<style scoped>
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
