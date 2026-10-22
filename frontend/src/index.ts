import type { PluginDefinition } from '@gameap/plugin-sdk';
import './styles.css';
import UpdaterPage from './components/UpdaterPage.vue';
import { PLUGIN_ID } from './lib/api';

// Single named export - the panel's bundle loader (and the Vite IIFE wrapper)
// expect exactly one exported PluginDefinition.
export const updaterPlugin: PluginDefinition = {
    // Must stay in step with pluginID in main.go.
    id: PLUGIN_ID,
    name: 'Updater',
    version: __PLUGIN_VERSION__,
    apiVersion: '1.0',
    description: 'Check and upgrade the GameAP panel, daemons and gameapctl',
    author: 'psinetreject',

    onInit() {
        if (!window.NaiveUI) console.error('Updater needs GameAP 4.4.0 or newer.');
    },

    routes: [
        {
            path: '/',
            name: 'index',
            component: UpdaterPage,
            meta: { title: 'Updater', requiresAuth: true, requiresAdmin: true },
        },
    ],

    menuItems: [
        {
            section: 'admin',
            icon: 'fas fa-arrows-rotate',
            text: 'Updater',
            route: { name: 'index' },
            order: 900,
            adminOnly: true,
        },
    ],
};
