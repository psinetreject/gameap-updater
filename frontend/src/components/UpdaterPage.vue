<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { NAlert, NButton, NCard, NSpin, NTable, NTag } from 'naive-ui';
import {
    getGameapctlLatest,
    getJob,
    getNodeGameapctl,
    getNodePanel,
    getNodes,
    getPluginStatus,
    getVersions,
    listJobs,
    startDaemonUpgrade,
    startGameapctlUpgrade,
    startPanelUpgrade,
} from '../lib/api';
import type { Job, NodeSummary, NodeVersion, PluginStatus, VersionResponse } from '../lib/api';

/**
 * Admin page: what is installed, what the latest releases are, and buttons
 * that upgrade the panel, the daemon and gameapctl on a node.
 *
 * Each source loads on its own, so one failing endpoint shows its own error
 * instead of blanking the page.
 */

type Component = Job['component'];

const COMPONENT_LABEL: Record<Component, string> = { gameapctl: 'gameapctl', daemon: 'gameap-daemon', panel: 'GameAP panel' };

const versions = ref<VersionResponse | null>(null);
const nodes = ref<NodeSummary[]>([]);
const status = ref<PluginStatus | null>(null);
const gameapctlLatest = ref<string | null>(null);
/** Keyed by node id; a node is absent while its command is still running. */
const gameapctl = ref<Record<number, NodeVersion>>({});
/** The panel binary on each node, keyed by node id; most nodes have none. */
const panelOn = ref<Record<number, NodeVersion>>({});
/** Set once every online node has answered whether it hosts a panel. */
const panelHostsChecked = ref(false);
const jobs = ref<Job[]>([]);
const errors = ref<string[]>([]);
const loading = ref(false);

/** Job ids being polled, so a job is never polled twice. */
const polling = new Set<string>();
let stopped = false;

const runningByNode = computed(() => {
    const byNode: Record<number, Job> = {};
    for (const job of jobs.value) if (job.state === 'running') byNode[job.node_id] = job;
    return byNode;
});

function nodeName(id: number): string {
    return nodes.value.find((n) => n.id === id)?.name ?? `node ${id}`;
}

function errMsg(source: string, e: unknown): string {
    const resp = (e as { response?: { status?: number; data?: { message?: string; error?: string } } }).response;
    const detail = resp?.data?.message ?? resp?.data?.error ?? (e as Error).message ?? String(e);
    return resp?.status ? `${source}: HTTP ${resp.status} ${detail}` : `${source}: ${detail}`;
}

async function load() {
    loading.value = true;
    errors.value = [];
    const [v, n, s] = await Promise.allSettled([getVersions(), getNodes(), getPluginStatus()]);
    if (v.status === 'fulfilled') versions.value = v.value;
    else errors.value.push(errMsg('Panel versions', v.reason));
    if (n.status === 'fulfilled') nodes.value = n.value;
    else errors.value.push(errMsg('Nodes', n.reason));
    if (s.status === 'fulfilled') status.value = s.value;
    else errors.value.push(errMsg('Plugin backend', s.reason));
    loading.value = false;
    loadGameapctl();
    loadPanelHosts();
    loadJobs();
}

/** Nodes with a panel binary; the one matching the running panel first. */
const panelHosts = computed(() => {
    const current = versions.value?.panel.current;
    return nodes.value
        .filter((n) => panelOn.value[n.id]?.version || panelOn.value[n.id]?.error)
        .sort((a, b) => Number(isThisPanel(b)) - Number(isThisPanel(a)));
    function isThisPanel(n: NodeSummary) {
        return !!current && stripV(panelOn.value[n.id]?.version ?? '') === stripV(current);
    }
});

/**
 * The plugin cannot ask the panel which machine it runs on, so every online
 * node is asked whether it has a panel binary. The page then trusts only the
 * node whose binary matches the version the running panel reports.
 */
async function loadPanelHosts() {
    panelOn.value = {};
    panelHostsChecked.value = false;
    await Promise.all(
        nodes.value
            .filter((node) => node.online)
            .map((node) =>
                getNodePanel(node.id)
                    .then((r) => (panelOn.value[node.id] = r))
                    .catch((e) => (panelOn.value[node.id] = { node_id: node.id, error: errMsg('Request', e) }))
            )
    );
    panelHostsChecked.value = true;
}

function isRunningPanel(node: NodeSummary): boolean {
    const current = versions.value?.panel.current;
    const v = panelOn.value[node.id]?.version;
    return !!current && !!v && stripV(v) === stripV(current);
}

async function loadJobs() {
    try {
        jobs.value = await listJobs();
    } catch (e) {
        errors.value.push(errMsg('Jobs', e));
        return;
    }
    for (const job of jobs.value) if (job.state === 'running') poll(job.id);
}

function upsertJob(job: Job) {
    const i = jobs.value.findIndex((j) => j.id === job.id);
    if (i >= 0) jobs.value[i] = job;
    else jobs.value.unshift(job);
}

/**
 * Polls a running job every 2 s until it finishes, then re-reads its node.
 * During a panel upgrade every poll fails for a while: the panel answering
 * them is being replaced.
 */
async function poll(id: string) {
    if (polling.has(id)) return;
    polling.add(id);
    try {
        while (!stopped) {
            await new Promise((r) => setTimeout(r, 2000));
            let job: Job;
            try {
                job = await getJob(id);
            } catch (e) {
                const code = (e as { response?: { status?: number } }).response?.status;
                if (code === 401 || code === 403) {
                    window.$message?.error('Signed out while waiting for the job; sign in and reload this page.');
                    break;
                }
                // A failed poll is not a failed job; try again next round.
                continue;
            }
            upsertJob(job);
            if (job.state !== 'running') {
                const label = COMPONENT_LABEL[job.component] ?? job.component;
                if (job.state === 'done') window.$message?.success(`${label} on ${nodeName(job.node_id)} is now ${job.to}`);
                else window.$message?.error(`${label} update on ${nodeName(job.node_id)} failed`);
                if (job.component === 'panel') {
                    if (job.state === 'done') window.$message?.success('Reload the page to load the new panel.');
                    const [v, p] = await Promise.all([getVersions().catch(() => null), getNodePanel(job.node_id).catch(() => null)]);
                    if (v) versions.value = v;
                    if (p) panelOn.value[job.node_id] = p;
                } else if (job.component === 'daemon') {
                    await refreshNodesUntil(job.node_id, job.state === 'done' ? job.to : null);
                } else {
                    const r = await getNodeGameapctl(job.node_id).catch(() => null);
                    if (r) gameapctl.value[job.node_id] = r;
                }
                break;
            }
        }
    } finally {
        polling.delete(id);
    }
}

const CHECKSUM =
    'The node first downloads the release from GitHub or a GameAP mirror and checks it against the SHA-256 GitHub lists for it; ' +
    'gameapctl only runs if it matches, and the installed binary is compared with it afterwards.';

function confirmText(node: NodeSummary, component: Component): string {
    if (component === 'panel') {
        const from = panelOn.value[node.id]?.version ?? '?';
        const to = versions.value?.panel.latest_stable ?? 'the latest release';
        return (
            `Upgrade the GameAP panel on ${node.name} from ${from} to ${to}? ` +
            'gameapctl stops the panel, replaces it, updates config.env and starts it again. ' +
            'If the new panel fails its health check, gameapctl puts the old binary and config.env back; ' +
            'the database is not part of that rollback, so take a backup first. ' +
            'The panel, this page included, is unavailable until it is back. ' +
            CHECKSUM
        );
    }
    if (component === 'daemon') {
        const from = node.version ?? '?';
        const to = versions.value?.daemon.latest_stable ?? 'the latest release';
        return (
            `Upgrade gameap-daemon on ${node.name} from ${from} to ${to}? ` +
            'gameapctl stops the daemon, replaces it and starts it again, and puts the old one back if the new one does not start. ' +
            'The node shows as offline until the daemon reconnects. ' +
            'Game servers run by the tmux or simple process manager may stop with the daemon; ' +
            'servers run by systemd, Docker or Podman have their own units. ' +
            CHECKSUM
        );
    }
    const from = gameapctl.value[node.id]?.version ?? '?';
    const to = gameapctlLatest.value ?? '?';
    return `Update gameapctl on ${node.name} from ${from} to ${to}? gameapctl downloads the release and replaces itself. ${CHECKSUM}`;
}

/**
 * The panel's node list carries the version a daemon reported when it last
 * connected, which trails a finished upgrade by some seconds. Re-reads it for
 * up to a minute until the node shows `version` (or just once without one).
 */
async function refreshNodesUntil(nodeId: number, version: string | null) {
    for (let i = 0; i < 20 && !stopped; i++) {
        const n = await getNodes().catch(() => null);
        if (n) nodes.value = n;
        const node = nodes.value.find((x) => x.id === nodeId);
        if (!version || (node?.version && stripV(node.version) === stripV(version))) return;
        await new Promise((r) => setTimeout(r, 3000));
    }
}

function confirmUpgrade(node: NodeSummary, component: Component) {
    const content = confirmText(node, component);
    if (!window.$dialog) {
        if (window.confirm(content)) void startUpgrade(node, component);
        return;
    }
    window.$dialog.warning({
        title: `Update ${COMPONENT_LABEL[component]}`,
        content,
        positiveText: 'Update',
        negativeText: 'Cancel',
        onPositiveClick: () => void startUpgrade(node, component),
    });
}

async function startUpgrade(node: NodeSummary, component: Component, unverified = false) {
    try {
        const start = { panel: startPanelUpgrade, daemon: startDaemonUpgrade, gameapctl: startGameapctlUpgrade }[component];
        const job = await start(node.id, unverified);
        upsertJob(job);
        if (job.state === 'running') poll(job.id);
        else window.$message?.error(job.error ?? 'The job did not start');
    } catch (e) {
        window.$message?.error(errMsg(`Update on ${node.name}`, e));
    }
}

/**
 * A failed job whose node could not verify the release offers the same
 * upgrade without verification, while it is still the newest job for that
 * component on that node.
 */
function canRetryUnverified(job: Job): boolean {
    if (job.state !== 'failed' || !job.cannot_verify || runningByNode.value[job.node_id]) return false;
    const newest = jobs.value.find((j) => j.node_id === job.node_id && j.component === job.component);
    return newest?.id === job.id;
}

function confirmUnverified(job: Job) {
    const node = nodes.value.find((n) => n.id === job.node_id);
    if (!node) return;
    const label = COMPONENT_LABEL[job.component];
    const content =
        `Upgrade ${label} on ${node.name} to ${job.to} WITHOUT verifying the download? ` +
        'The node could not check the release (see the job log for why). gameapctl will download it from ' +
        'whichever source answers fastest and install it unchecked. Only do this if you trust the node\'s network.';
    if (!window.$dialog) {
        if (window.confirm(content)) void startUpgrade(node, job.component, true);
        return;
    }
    window.$dialog.warning({
        title: `Upgrade ${label} without verification`,
        content,
        positiveText: 'Upgrade unverified',
        negativeText: 'Cancel',
        onPositiveClick: () => void startUpgrade(node, job.component, true),
    });
}

/** One job per node at a time, whichever component it upgrades. */
function canUpgrade(node: NodeSummary, component: Component): boolean {
    if (!status.value?.upgrades || runningByNode.value[node.id]) return false;
    if (component === 'panel') {
        const latest = versions.value?.panel.latest_stable;
        return isRunningPanel(node) && !!versions.value?.panel.update_available && !!latest;
    }
    const state = component === 'daemon' ? daemonState(node) : gameapctlState(node);
    return state.text === 'Update available';
}

function formatTime(unix?: number): string {
    return unix ? new Date(unix * 1000).toLocaleString() : '-';
}

function jobTag(job: Job): 'success' | 'error' | 'info' {
    return job.state === 'done' ? 'success' : job.state === 'failed' ? 'error' : 'info';
}

/**
 * gameapctl's version needs a command on each node, which takes seconds and can
 * fail per node, so it loads after the rest and fills in row by row.
 */
async function loadGameapctl() {
    gameapctl.value = {};
    getGameapctlLatest()
        .then((tag) => (gameapctlLatest.value = tag))
        .catch((e) => errors.value.push(errMsg('gameapctl latest release', e)));
    await Promise.all(
        nodes.value
            .filter((node) => node.online)
            .map((node) =>
                getNodeGameapctl(node.id)
                    .then((r) => (gameapctl.value[node.id] = r))
                    .catch((e) => (gameapctl.value[node.id] = { node_id: node.id, error: errMsg('Request', e) }))
            )
    );
}

function gameapctlState(node: NodeSummary): { text: string; type: 'success' | 'warning' | 'error' | 'default' } {
    if (!node.online) return { text: 'Offline', type: 'default' };
    const r = gameapctl.value[node.id];
    if (!r) return { text: 'Checking...', type: 'default' };
    if (r.error || !r.version) return { text: 'Error', type: 'error' };
    if (!gameapctlLatest.value) return { text: 'Unknown', type: 'default' };
    return stripV(r.version) === stripV(gameapctlLatest.value)
        ? { text: 'Up to date', type: 'success' }
        : { text: 'Update available', type: 'warning' };
}

/** A node is behind when its version differs from the latest daemon release. */
function daemonState(node: NodeSummary): { text: string; type: 'success' | 'warning' | 'default' } {
    const latest = versions.value?.daemon.latest_stable;
    if (!node.online) return { text: 'Offline', type: 'default' };
    if (!node.version || !latest) return { text: 'Unknown', type: 'default' };
    if (node.outdated || stripV(node.version) !== stripV(latest)) return { text: 'Update available', type: 'warning' };
    return { text: 'Up to date', type: 'success' };
}

function stripV(v: string): string {
    return v.replace(/^v/, '');
}

onMounted(load);
onBeforeUnmount(() => (stopped = true));
</script>

<template>
    <div class="gu-root">
        <div class="gu-header">
            <h1 class="gu-title">Updater</h1>
            <NButton size="small" :loading="loading" @click="load">Refresh</NButton>
        </div>

        <NAlert v-for="e in errors" :key="e" type="error" class="gu-gap">{{ e }}</NAlert>

        <NSpin :show="loading && !versions">
            <NCard title="Panel" class="gu-gap">
                <NTable v-if="versions" size="small" :single-line="false">
                    <thead>
                        <tr><th>Installed</th><th>Latest stable</th><th>Latest beta</th><th>Status</th></tr>
                    </thead>
                    <tbody>
                        <tr>
                            <td class="gu-mono">{{ versions.panel.current }}</td>
                            <td class="gu-mono">{{ versions.panel.latest_stable ?? '-' }}</td>
                            <td class="gu-mono">{{ versions.panel.latest_beta ?? '-' }}</td>
                            <td>
                                <NTag v-if="versions.panel.update_available" type="warning" size="small">Update available</NTag>
                                <NTag v-else type="success" size="small">Up to date</NTag>
                            </td>
                        </tr>
                    </tbody>
                </NTable>
                <p v-if="versions && !versions.update_check_enabled" class="gu-muted">
                    The panel's update check is turned off, so the latest versions may be missing.
                </p>

                <NTable v-if="panelHosts.length" size="small" :single-line="false" class="gu-subtable">
                    <thead>
                        <tr><th>Host node</th><th>Panel binary</th><th></th><th></th></tr>
                    </thead>
                    <tbody>
                        <tr v-for="node in panelHosts" :key="node.id">
                            <td>{{ node.name }}</td>
                            <td class="gu-mono">
                                {{ panelOn[node.id]?.version ?? '-' }}
                                <div v-if="panelOn[node.id]?.error" class="gu-muted">{{ panelOn[node.id]?.error }}</div>
                            </td>
                            <td>
                                <NTag v-if="isRunningPanel(node)" type="info" size="small">This panel</NTag>
                                <NTag v-else size="small">Another panel</NTag>
                            </td>
                            <td>
                                <NButton v-if="runningByNode[node.id]?.component === 'panel'" size="tiny" loading disabled>Updating</NButton>
                                <NButton v-else-if="canUpgrade(node, 'panel')" size="tiny" type="primary" @click="confirmUpgrade(node, 'panel')">Update</NButton>
                            </td>
                        </tr>
                    </tbody>
                </NTable>
                <p v-else-if="panelHostsChecked && versions" class="gu-muted">
                    None of the nodes hosts this panel, so it cannot be upgraded from here. Run
                    <span class="gu-mono">gameapctl panel upgrade</span> on the machine it runs on.
                </p>
            </NCard>

            <NCard title="Daemons" class="gu-gap">
                <NTable v-if="nodes.length" size="small" :single-line="false">
                    <thead>
                        <tr><th>Node</th><th>Installed</th><th>Latest stable</th><th>Status</th><th></th></tr>
                    </thead>
                    <tbody>
                        <tr v-for="node in nodes" :key="node.id">
                            <td>{{ node.name }}</td>
                            <td class="gu-mono">{{ node.version ?? '-' }}</td>
                            <td class="gu-mono">{{ versions?.daemon.latest_stable ?? '-' }}</td>
                            <td><NTag :type="daemonState(node).type" size="small">{{ daemonState(node).text }}</NTag></td>
                            <td>
                                <NButton v-if="runningByNode[node.id]?.component === 'daemon'" size="tiny" loading disabled>Updating</NButton>
                                <NButton v-else-if="canUpgrade(node, 'daemon')" size="tiny" type="primary" @click="confirmUpgrade(node, 'daemon')">Update</NButton>
                            </td>
                        </tr>
                    </tbody>
                </NTable>
                <p v-else-if="!loading" class="gu-muted">No nodes.</p>
            </NCard>

            <NCard title="gameapctl" class="gu-gap">
                <NTable v-if="nodes.length" size="small" :single-line="false">
                    <thead>
                        <tr><th>Node</th><th>Installed</th><th>Latest stable</th><th>Status</th><th></th></tr>
                    </thead>
                    <tbody>
                        <tr v-for="node in nodes" :key="node.id">
                            <td>{{ node.name }}</td>
                            <td class="gu-mono">
                                {{ gameapctl[node.id]?.version ?? '-' }}
                                <div v-if="gameapctl[node.id]?.error" class="gu-muted">{{ gameapctl[node.id]?.error }}</div>
                            </td>
                            <td class="gu-mono">{{ gameapctlLatest ?? '-' }}</td>
                            <td><NTag :type="gameapctlState(node).type" size="small">{{ gameapctlState(node).text }}</NTag></td>
                            <td>
                                <NButton v-if="runningByNode[node.id]?.component === 'gameapctl'" size="tiny" loading disabled>Updating</NButton>
                                <NButton v-else-if="canUpgrade(node, 'gameapctl')" size="tiny" type="primary" @click="confirmUpgrade(node, 'gameapctl')">Update</NButton>
                            </td>
                        </tr>
                    </tbody>
                </NTable>
                <p v-else-if="!loading" class="gu-muted">No nodes.</p>
            </NCard>
            <NCard title="Recent jobs" class="gu-gap">
                <NTable v-if="jobs.length" size="small" :single-line="false">
                    <thead>
                        <tr><th>Started</th><th>Node</th><th>Component</th><th>Change</th><th>State</th></tr>
                    </thead>
                    <tbody>
                        <tr v-for="job in jobs" :key="job.id">
                            <td>{{ formatTime(job.started_at) }}</td>
                            <td>{{ nodeName(job.node_id) }}</td>
                            <td>{{ job.component }}</td>
                            <td class="gu-mono">{{ job.from }} → {{ job.to }}</td>
                            <td>
                                <NTag :type="jobTag(job)" size="small">{{ job.state }}</NTag>
                                <div v-if="job.error" class="gu-muted">{{ job.error }}</div>
                                <div v-if="job.unverified" class="gu-muted">Not verified: started without checking the download.</div>
                                <NButton v-if="canRetryUnverified(job)" size="tiny" class="gu-retry" @click="confirmUnverified(job)">
                                    Update without verification
                                </NButton>
                                <div v-if="job.state === 'done' && job.archive_sha256" class="gu-muted">
                                    SHA-256 verified: <span class="gu-mono" :title="job.archive_sha256">{{ job.asset }} · {{ job.archive_sha256.slice(0, 12) }}…</span>
                                </div>
                                <div v-if="job.note" class="gu-muted">{{ job.note }}</div>
                                <details v-if="job.log" class="gu-log">
                                    <summary>Log</summary>
                                    <pre class="gu-mono gu-pre">{{ job.log }}</pre>
                                </details>
                            </td>
                        </tr>
                    </tbody>
                </NTable>
                <p v-else-if="!loading" class="gu-muted">No jobs yet.</p>
            </NCard>
        </NSpin>

        <p v-if="status" class="gu-muted gu-footer">
            Plugin {{ status.version }} · upgrades {{ status.upgrades ? 'enabled' : 'not available in this build' }}
        </p>
    </div>
</template>
