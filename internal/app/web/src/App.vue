<script setup lang="ts">
import UApp from '@nuxt/ui/components/App.vue'
import AppLayout from './layout/AppLayout.vue'
</script>

<template>
  <!-- The app shows its own toasts (MessageToasts.vue) in its one live region. -->
  <UApp :toaster="null">
    <AppLayout />
  </UApp>
</template>
