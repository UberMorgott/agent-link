<script setup lang="ts">
import UApp from '@nuxt/ui/components/App.vue'
import AppLayout from './layout/AppLayout.vue'
</script>

<template>
  <!-- News of a message and failures of an action both come in Nuxt UI's
       toasts (useToast). -->
  <UApp>
    <AppLayout />
  </UApp>
</template>
