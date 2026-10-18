<script setup lang="ts">
import { RouterView } from 'vue-router'
import SideBar from './SideBar.vue'
import TopBar from './TopBar.vue'
import Toast from '../ui/Toast.vue'
</script>

<template>
  <div class="shell">
    <SideBar class="shell__side" />
    <TopBar class="shell__top" />
    <main class="shell__main">
      <RouterView v-slot="{ Component, route }">
        <transition name="fade" mode="out-in">
          <component :is="Component" :key="route.fullPath" />
        </transition>
      </RouterView>
    </main>
    <Toast />
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: var(--sidebar-w) 1fr;
  grid-template-rows: var(--topbar-h) 1fr;
  grid-template-areas:
    'side top'
    'side main';
  height: 100vh;
  background: var(--bg);
}
.shell__side { grid-area: side; }
.shell__top { grid-area: top; }
.shell__main {
  grid-area: main;
  padding: var(--space-6);
  overflow: auto;
  min-width: 0;
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity var(--dur-fast) var(--ease-out);
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>