<script setup lang="ts">
import UApp from '@nuxt/ui/components/App.vue'
import AppLayout from './layout/AppLayout.vue'
</script>

<template>
  <!-- News comes in the app's own toasts (MessageToasts.vue); failures of an
       action in Nuxt UI's (useToast). -->
  <UApp>
    <AppLayout />
  </UApp>
</template>
