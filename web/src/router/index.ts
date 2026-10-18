/**
 * vue-router with role-aware guards. `meta.requiresRole` is the canonical
 * gate; missing the role sends the user to /forbidden with a hint of where
 * they were trying to go.
 */

import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('../views/LoginView.vue'),
    meta: { public: true },
  },
  {
    path: '/',
    component: () => import('../components/layout/LayoutShell.vue'),
    meta: { requiresRole: 'viewer' as const },
    children: [
      {
        path: '',
        name: 'dashboard',
        component: () => import('../views/TasksListView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
      {
        path: 'tasks',
        name: 'tasks',
        component: () => import('../views/TasksListView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
      {
        path: 'tasks/new',
        name: 'task-new',
        component: () => import('../views/TaskFormView.vue'),
        meta: { requiresRole: 'admin' as const },
      },
      {
        path: 'tasks/:id',
        name: 'task-detail',
        component: () => import('../views/TaskDetailView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
      {
        path: 'tasks/:id/runs',
        name: 'task-runs',
        component: () => import('../views/TaskRunsView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
      {
        path: 'tasks/:id/edit',
        name: 'task-edit',
        component: () => import('../views/TaskFormView.vue'),
        meta: { requiresRole: 'admin' as const },
      },
      {
        path: 'users',
        name: 'users',
        component: () => import('../views/UsersView.vue'),
        meta: { requiresRole: 'admin' as const },
      },
      {
        path: 'profile',
        name: 'profile',
        component: () => import('../views/ProfileView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
      {
        path: 'metrics',
        name: 'metrics',
        component: () => import('../views/MetricsView.vue'),
        meta: { requiresRole: 'viewer' as const },
      },
    ],
  },
  {
    path: '/forbidden',
    name: 'forbidden',
    component: () => import('../views/ForbiddenView.vue'),
    meta: { public: true },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('../views/NotFoundView.vue'),
    meta: { public: true },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach((to, _from, next) => {
  const auth = useAuthStore()
  const requiresRole = to.matched.find((r) => r.meta.requiresRole)?.meta.requiresRole as
    | 'viewer'
    | 'admin'
    | undefined

  if (!requiresRole) return next()
  if (!auth.isAuthenticated) {
    return next({ path: '/login', query: { next: to.fullPath } })
  }
  if (requiresRole === 'admin' && !auth.isAdmin) {
    return next({ path: '/forbidden', query: { from: to.fullPath } })
  }
  return next()
})