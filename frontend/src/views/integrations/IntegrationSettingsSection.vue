<template>
  <div class="integrations-settings">
    <div class="integrations-settings__body">
      <div v-if="tab === 'api'" class="section">
        <div class="section-header">
          <h2>{{ $t('integrations.api.title') }}</h2>
          <p class="section-description">{{ $t('integrations.api.subtitle') }}</p>
        </div>
        <ApiIntegrationSettings />
      </div>

      <div v-if="tab === 'mcpserver'" class="section">
        <div class="section-header">
          <h2>{{ $t('integrations.mcpserver.title') }}</h2>
          <p class="section-description">{{ $t('integrations.mcpserver.subtitle') }}</p>
        </div>
        <McpServerIntegrationSettings />
      </div>

      <ChromeExtensionLanding v-if="tab === 'chrome'" />
      <CliIntegrationLanding v-if="tab === 'cli'" />
    </div>
  </div>
</template>

<script setup lang="ts">
import ApiIntegrationSettings from '@/views/integrations/ApiIntegrationSettings.vue'
import McpServerIntegrationSettings from '@/views/integrations/McpServerIntegrationSettings.vue'
import ChromeExtensionLanding from '@/views/integrations/ChromeExtensionLanding.vue'
import CliIntegrationLanding from '@/views/integrations/CliIntegrationLanding.vue'
import type { IntegrationTab } from '@/config/integrations'

defineProps<{
  tab: IntegrationTab
}>()
</script>

<style scoped lang="less">
@import (reference) '@/components/css/settings-section.less';

.integrations-settings {
  display: flex;
  flex-direction: column;
}

.integrations-settings__body {
  min-width: 0;
}

.section-header {
  .settings-section-header();
}

.doc-link {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  margin-left: 6px;
  color: var(--td-brand-color);
  text-decoration: none;

  &:hover {
    text-decoration: underline;
  }
}

.link-icon {
  font-size: var(--app-text-md);
}
</style>
