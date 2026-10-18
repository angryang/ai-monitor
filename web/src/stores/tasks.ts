/**
 * Tasks store. Caches the last-fetched list so navigation between views
 * stays snappy and offline retries have something to fall back to.
 */

import { defineStore } from 'pinia'
import { tasksApi, type Task, type TaskWritePayload } from '../api/tasks'

interface State {
  tasks: Task[]
  loading: boolean
  lastError: string | null
}

export const useTasksStore = defineStore('tasks', {
  state: (): State => ({ tasks: [], loading: false, lastError: null }),
  actions: {
    async fetchAll() {
      this.loading = true
      this.lastError = null
      try {
        const { tasks } = await tasksApi.list()
        this.tasks = tasks
      } catch (e) {
        this.lastError = (e as Error).message
      } finally {
        this.loading = false
      }
    },
    async create(payload: TaskWritePayload): Promise<Task> {
      const t = await tasksApi.create(payload)
      this.tasks = [t, ...this.tasks]
      return t
    },
    async update(id: string, payload: Partial<TaskWritePayload>): Promise<Task> {
      const t = await tasksApi.update(id, payload)
      this.tasks = this.tasks.map((x) => (x.id === id ? t : x))
      return t
    },
    async remove(id: string) {
      await tasksApi.remove(id)
      this.tasks = this.tasks.filter((x) => x.id !== id)
    },
  },
})