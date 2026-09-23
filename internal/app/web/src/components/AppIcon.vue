<script setup lang="ts">
import { computed } from 'vue'

// Inline line icons drawn with currentColor: a handful of shapes instead of an
// icon font keeps the binary small.
type Shape = ['path', { d: string }] | ['circle', { cx: number; cy: number; r: number }]
  | ['rect', { x: number; y: number; width: number; height: number; rx: number }]

const p = (d: string): Shape => ['path', { d }]
const c = (cx: number, cy: number, r: number): Shape => ['circle', { cx, cy, r }]
const r = (x: number, y: number, width: number, height: number, rx: number): Shape => ['rect', { x, y, width, height, rx }]

const ICONS: Record<string, { box: string; shapes: Shape[] }> = {
  dashboard: { box: '0 0 20 20', shapes: [r(3, 3, 5.5, 5.5, 1.5), r(11.5, 3, 5.5, 5.5, 1.5), r(3, 11.5, 5.5, 5.5, 1.5), r(11.5, 11.5, 5.5, 5.5, 1.5)] },
  inbox: { box: '0 0 20 20', shapes: [p('M4 4.5h12a1.5 1.5 0 0 1 1.5 1.5v7a1.5 1.5 0 0 1-1.5 1.5H9l-3.5 3v-3H4A1.5 1.5 0 0 1 2.5 13V6A1.5 1.5 0 0 1 4 4.5z')] },
  participants: { box: '0 0 20 20', shapes: [c(7.5, 7, 3), p('M2 16.5c.6-2.8 2.8-4.5 5.5-4.5s4.9 1.7 5.5 4.5'), p('M13 4.2a3 3 0 0 1 0 5.6M15 12.3c1.5.6 2.6 2.1 3 4.2')] },
  settings: { box: '0 0 20 20', shapes: [c(10, 10, 2.6), p('M10 2.5v2.2M10 15.3v2.2M2.5 10h2.2M15.3 10h2.2M4.7 4.7l1.6 1.6M13.7 13.7l1.6 1.6M4.7 15.3l1.6-1.6M13.7 6.3l1.6-1.6')] },
  plus: { box: '0 0 20 20', shapes: [p('M10 4v12M4 10h12')] },
  archive: { box: '0 0 20 20', shapes: [r(2.5, 3.5, 15, 4, 1), p('M4 7.5v8a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1v-8M8 11h4')] },
  back: { box: '0 0 20 20', shapes: [p('M12.5 4.5 7 10l5.5 5.5')] },
  info: { box: '0 0 20 20', shapes: [c(10, 10, 7.5), p('M10 9v5M10 6.2v.1')] },
  send: { box: '0 0 20 20', shapes: [p('M10 16V4.5M5 9.5l5-5 5 5')] },
  menu: { box: '0 0 20 20', shapes: [p('M3.5 6h13M3.5 10h13M3.5 14h13')] },
  sun: { box: '0 0 20 20', shapes: [c(10, 10, 3.2), p('M10 2v2M10 16v2M2 10h2M16 10h2M4.3 4.3l1.4 1.4M14.3 14.3l1.4 1.4M4.3 15.7l1.4-1.4M14.3 5.7l1.4-1.4')] },
  moon: { box: '0 0 20 20', shapes: [p('M16 12.2A6.5 6.5 0 0 1 7.8 4a6.5 6.5 0 1 0 8.2 8.2z')] },
  system: { box: '0 0 20 20', shapes: [r(2.5, 3.5, 15, 10, 1.5), p('M7 16.5h6M10 13.5v3')] },
  // Chat marks: delivery ticks and who wrote a message.
  queued: { box: '0 0 16 16', shapes: [c(8, 8, 5.6), p('M8 5v3.2l2 1.3')] },
  delivered: { box: '0 0 16 16', shapes: [p('M3.5 8.4l2.9 2.9 6.1-6.6')] },
  read: { box: '0 0 20 16', shapes: [p('M1.8 8.4l2.9 2.9 6.1-6.6M8.6 10.6l.7.7 6.1-6.6')] },
  held: { box: '0 0 16 16', shapes: [c(8, 8, 5.6), p('M8 5v3.6M8 10.8v.1')] },
  agent: { box: '0 0 16 16', shapes: [r(3, 5, 10, 8, 2.2), p('M8 2.5V5M6 9h.01M10 9h.01')] },
  human: { box: '0 0 16 16', shapes: [c(8, 5.6, 2.6), p('M3.2 13.5c.6-2.4 2.5-3.8 4.8-3.8s4.2 1.4 4.8 3.8')] },
}

const props = defineProps<{ name: string }>()
const icon = computed(() => ICONS[props.name] ?? { box: '0 0 20 20', shapes: [] })
</script>

<template>
  <svg
    class="app-icon"
    :viewBox="icon.box"
    aria-hidden="true"
  >
    <component
      :is="shape[0]"
      v-for="(shape, i) in icon.shapes"
      :key="i"
      v-bind="shape[1]"
    />
  </svg>
</template>

<style scoped>
.app-icon {
  width: var(--icon-size, 1.1rem); height: var(--icon-size, 1.1rem); flex: none;
  fill: none; stroke: currentColor; stroke-width: 1.6; stroke-linecap: round; stroke-linejoin: round;
}
</style>
