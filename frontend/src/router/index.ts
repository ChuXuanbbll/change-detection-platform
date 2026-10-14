import { createRouter, createWebHistory } from 'vue-router';
import Overview from '../views/Overview.vue';

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', name: 'overview', component: Overview },
    { path: '/comparison', name: 'comparison', component: () => import('../views/Comparison.vue') },
    { path: '/management', name: 'management', component: () => import('../views/Management.vue') },
    { path: '/reports', name: 'reports', component: () => import('../views/Reports.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/overview' }
  ]
});

export default router;
