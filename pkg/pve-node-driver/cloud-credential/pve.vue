<script>
import Banner from '@components/Banner/Banner';
import { Checkbox } from '@components/Form/Checkbox';
import { LabeledInput } from '@components/Form/LabeledInput';
import { parse as parseUrl } from '@shell/utils/url';

export default {
  components: {
    Banner,
    Checkbox,
    LabeledInput,
  },
  props: {
    mode: {
      type:     String,
      required: true,
    },
    value: {
      type:     Object,
      required: true,
    },
  },
  data: () => ({
    errorLabelKey: null,
    whitelistHost: '',
    whitelistSaving: false,
    whitelistError: '',
  }),
  created() {
    this.value.setData('url', this.value.decodedData.url ?? '');
    this.value.setData('insecureTls', this.value.decodedData.insecureTls ?? false);
    this.value.setData('tokenId', this.value.decodedData.tokenId ?? '');
    this.value.setData('tokenSecret', this.value.decodedData.tokenSecret ?? '');

    this.validate();
  },
  methods: {
    validate(){
      if(this.value.decodedData.url == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(this.value.decodedData.tokenId == '') {
        this.$emit('validationChanged', false);
        return
      }

      if(this.value.decodedData.tokenSecret == '') {
        this.$emit('validationChanged', false);
        return
      }

      this.$emit('validationChanged', true);
    },
    async test() {
      this.errorLabelKey = null;
      this.whitelistHost = '';
      this.whitelistError = '';

      // Proxmox VE domain must be present in driver's whitelisted domains
      try {
        const nodeDriver = await this.$store.dispatch('rancher/find', {
          type: 'nodedriver',
          id:   'pve'
        });

        const domain = parseUrl(this.value.decodedData.url).host;
        const whitelistedDomains = nodeDriver.whitelistDomains ?? [];

        if(!whitelistedDomains.includes(domain)) {
          this.errorLabelKey = 'cluster.credential.pve.errors.whitelistedDomains';
          this.whitelistHost = domain;
          return false;
        }
      } catch(e) {
        this.errorLabelKey = 'cluster.credential.pve.errors.fetchNodeDriver';
        return false;
      }

      // Proxmox VE version must be 8.x
      if(!this.value.decodedData.insecureTls) {
        try {
          const { data } = await this.$store.dispatch('management/request', {
            method: 'GET',
            url: '/meta/proxy/' + this.value.decodedData.url.replace(/^https?:\/\//, '') + '/api2/json/version',
            headers: {
              "X-API-Auth-Header": `PVEAPIToken=${this.value.decodedData.tokenId}=${this.value.decodedData.tokenSecret}`,
            },
            redirectUnauthorized: false,
          })
  
          if(!data.version.startsWith('8.') && !data.version.startsWith('9.')) {
            this.errorLabelKey = 'cluster.credential.pve.errors.unsupportedProxmoxVersion';
            return false;
          }
        } catch(e) {
          if(e._status == 401) {
            this.errorLabelKey = 'cluster.credential.pve.errors.fetchProxmoxVersionUnauthorized';
          } else {
            this.errorLabelKey = 'cluster.credential.pve.errors.fetchProxmoxVersion';
          }
  
          return false;
        }
      }

      return true;
    },
    async addHostToWhitelist() {
      if(!this.whitelistHost || this.whitelistSaving) {
        return false;
      }
      this.whitelistSaving = true;
      this.whitelistError = '';
      try {
        const nodeDriver = await this.$store.dispatch('rancher/find', {
          type: 'nodedriver',
          id: 'pve',
        });
        const whitelist = nodeDriver.whitelistDomains || [];
        if(!whitelist.includes(this.whitelistHost)) {
          nodeDriver.whitelistDomains = [...whitelist, this.whitelistHost];
          await nodeDriver.save();
        }
        this.whitelistHost = '';
        return await this.test();
      } catch(_error) {
        this.whitelistError = 'Could not update the node driver allowlist. Ask a Rancher administrator to approve this host.';
        return false;
      } finally {
        this.whitelistSaving = false;
      }
    }
  }
}
</script>

<template>
  <div>
    <div class="mb-20" v-if="errorLabelKey">
      <Banner
        color="error"
        :label-key="errorLabelKey"
      />
    </div>
    <div v-if="whitelistHost" class="mb-20">
      <Banner color="warning">
        Proxmox host {{ whitelistHost }} is not in the node driver allowlist.
        <button type="button" class="btn role-secondary ml-10" :disabled="whitelistSaving" @click="addHostToWhitelist">
          {{ whitelistSaving ? 'Adding host…' : 'Add host to allowlist and retry' }}
        </button>
      </Banner>
    </div>
    <div v-if="whitelistError" class="mb-20">
      <Banner color="error">{{ whitelistError }}</Banner>
    </div>
    <div class="mb-20">
      <LabeledInput
        type="text"
        :mode="mode"
        :value="value.decodedData.url"
        @change="e => {value.setData('url', e.target.value); validate();}"
        label-key="cluster.credential.pve.url.label"
        placeholder-key="cluster.credential.pve.url.placeholder"
        required
      />
    </div>
    <div class="mb-20">
      <Checkbox
        :mode="mode"
        :value="value.decodedData.insecureTls"
        @click="e => {value.setData('insecureTls', !value.decodedData.insecureTls); validate();}"
        label-key="cluster.credential.pve.insecureTLS.label"
      />
      <Banner
        v-if="value.decodedData.insecureTls"
        color="warning"
        label-key="cluster.credential.pve.insecureTLS.warning"
      />
    </div>
    <div class="mb-20">
      <LabeledInput
        type="text"
        :mode="mode"
        :value="value.decodedData.tokenId"
        @change="e => {value.setData('tokenId', e.target.value); validate();}"
        label-key="cluster.credential.pve.tokenID.label"
        placeholder-key="cluster.credential.pve.tokenID.placeholder"
        tooltip-key="cluster.credential.pve.tokenID.tooltip"
        required
      />
    </div>
    <div>
      <LabeledInput
        type="password"
        :mode="mode"
        :value="value.decodedData.tokenSecret"
        @change="e => {value.setData('tokenSecret', e.target.value); validate();}"
        label-key="cluster.credential.pve.tokenSecret.label"
        placeholder-key="cluster.credential.pve.tokenSecret.placeholder"
        required
      />
    </div>
  </div>
</template>
