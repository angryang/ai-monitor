<script setup lang="ts" generic="T extends { id: string }">
import { ref } from 'vue'
import Drawer from './Drawer.vue'

interface Column<Row> {
  key: string
  label: string
  width?: string
  render?: (row: Row) => unknown
}
interface Props {
  rows: T[]
  columns: Column<T>[]
  hoverDetail?: (row: T) => unknown
  empty?: string
}
const props = withDefaults(defineProps<Props>(), { empty: 'no rows' })

const hovered = ref<T | null>(null)
const drawerOpen = ref(false)

function onRowEnter(row: T) {
  hovered.value = row
  if (props.hoverDetail) drawerOpen.value = true
}
function onRowLeave() {
  // drawer stays open briefly so users can interact with it; closing is
  // handled by Drawer's own close affordance.
}
</script>

<template>
  <div class="dt">
    <table class="dt__table">
      <thead>
        <tr>
          <th v-for="c in columns" :key="c.key" :style="c.width ? { width: c.width } : undefined">
            {{ c.label }}
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="rows.length === 0">
          <td :colspan="columns.length" class="dt__empty text-muted">{{ empty }}</td>
        </tr>
        <tr
          v-for="row in rows"
          :key="row.id"
          class="dt__row"
          @mouseenter="onRowEnter(row)"
          @mouseleave="onRowLeave"
        >
          <td v-for="c in columns" :key="c.key">
            <slot :name="`cell-${c.key}`" :row="row">
              {{ c.render ? c.render(row) : (row as any)[c.key] }}
            </slot>
          </td>
        </tr>
      </tbody>
    </table>
    <Drawer :open="drawerOpen" :title="hovered ? `Inspect · ${(hovered as any).name ?? (hovered as any).username ?? (hovered as any).id}` : ''" @close="drawerOpen = false">
      <slot v-if="hovered" name="drawer" :row="hovered">
        <pre class="dt__dump">{{ hovered }}</pre>
      </slot>
    </Drawer>
  </div>
</template>

<style scoped>
.dt { overflow: auto; border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--surface); }
.dt__table { width: 100%; border-collapse: collapse; font-size: var(--fs-13); }
.dt__table th {
  text-align: left;
  font-weight: var(--fw-medium);
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
  position: sticky;
  top: 0;
}
.dt__table td {
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border);
  vertical-align: middle;
}
.dt__row { transition: background var(--dur-fast) var(--ease-out); }
.dt__row:hover { background: var(--surface-2); }
.dt__empty {
  text-align: center;
  padding: var(--space-6);
  font-style: italic;
}
.dt__dump {
  font-family: var(--font-mono);
  font-size: var(--fs-12);
  color: var(--muted);
  white-space: pre-wrap;
  word-break: break-word;
}
</style>