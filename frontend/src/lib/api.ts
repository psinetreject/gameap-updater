import axios from 'axios';

/**
 * The panel endpoints this page reads. Both are admin-only on the panel side,
 * which matches the page itself.
 *
 *   GET /api/version        panel current + latest releases, daemon latest
 *   GET /api/nodes/summary  every node with its daemon version
 *
 * Shapes are copied from gameap v4.5.8 internal/api/version/getversion and
 * internal/api/nodes/getsummary; keep them in step by hand.
 */

export interface ReleaseInfo {
    latest_stable?: string;
    latest_stable_url?: string;
    latest_beta?: string;
    latest_beta_url?: string;
}

export interface VersionResponse {
    panel: ReleaseInfo & {
        current: string;
        build_date: string;
        is_release: boolean;
        update_available: boolean;
    };
    daemon: ReleaseInfo;
    update_check_enabled: boolean;
}

export interface NodeSummary {
    id: number;
    name: string;
    location: string;
    enabled: boolean;
    online: boolean;
    version?: string;
    buildDate?: string;
    outdated?: boolean;
}

export interface NodesSummaryResponse {
    total: number;
    online: number;
    offline: number;
    onlineNodes: NodeSummary[];
    offlineNodes: NodeSummary[];
}

/** What this plugin's own backend reports (main.go statusResponse). */
export interface PluginStatus {
    plugin: string;
    version: string;
    upgrades: boolean;
}

/** A program's version on one node, as main.go nodeVersionResponse reports it. */
export interface NodeVersion {
    node_id: number;
    os?: string;
    version?: string;
    /** The node does not have the program; normal for the panel. */
    not_installed?: boolean;
    error?: string;
}

/** An upgrade job, as internal/jobs.Job stores it. */
export interface Job {
    id: string;
    component: 'gameapctl' | 'daemon' | 'panel';
    node_id: number;
    from: string;
    to: string;
    state: 'running' | 'done' | 'failed';
    started_at: number;
    finished_at?: number;
    actor_id?: number;
    error?: string;
    log?: string;
    /** The release archive the job checks, and GitHub's SHA-256 for it. */
    asset?: string;
    archive_sha256?: string;
    /** Started without verification, on an admin's request. */
    unverified?: boolean;
    /** Stopped because the node could not verify; upgrading unverified is offered. */
    cannot_verify?: boolean;
    /** Set when a running job's node could not be reached this time. */
    note?: string;
}

/** Must stay in step with pluginID in main.go, which explains its form. */
export const PLUGIN_ID = 'amfatvdydz4ym';

export async function getVersions(): Promise<VersionResponse> {
    return (await axios.get<VersionResponse>('/api/version')).data;
}

export async function getNodes(): Promise<NodeSummary[]> {
    const data = (await axios.get<NodesSummaryResponse>('/api/nodes/summary')).data;
    return [...(data.onlineNodes ?? []), ...(data.offlineNodes ?? [])];
}

export async function getPluginStatus(): Promise<PluginStatus> {
    return (await axios.get<PluginStatus>(`/api/plugins/${PLUGIN_ID}/status`)).data;
}

export async function getGameapctlLatest(): Promise<string> {
    return (await axios.get<{ latest: string }>(`/api/plugins/${PLUGIN_ID}/gameapctl/latest`)).data.latest;
}

export async function getNodeGameapctl(nodeId: number): Promise<NodeVersion> {
    return (await axios.get<NodeVersion>(`/api/plugins/${PLUGIN_ID}/nodes/${nodeId}/gameapctl`)).data;
}

export async function getNodePanel(nodeId: number): Promise<NodeVersion> {
    return (await axios.get<NodeVersion>(`/api/plugins/${PLUGIN_ID}/nodes/${nodeId}/panel`)).data;
}

export async function startPanelUpgrade(nodeId: number, unverified = false): Promise<Job> {
    return (await axios.post<Job>(upgradeUrl(nodeId, 'panel', unverified))).data;
}

export async function startGameapctlUpgrade(nodeId: number, unverified = false): Promise<Job> {
    return (await axios.post<Job>(upgradeUrl(nodeId, 'gameapctl', unverified))).data;
}

/** `?unverified=1` is only accepted after a job reported cannot_verify. */
function upgradeUrl(nodeId: number, component: Job['component'], unverified: boolean): string {
    return `/api/plugins/${PLUGIN_ID}/nodes/${nodeId}/${component}/upgrade${unverified ? '?unverified=1' : ''}`;
}

export async function startDaemonUpgrade(nodeId: number, unverified = false): Promise<Job> {
    return (await axios.post<Job>(upgradeUrl(nodeId, 'daemon', unverified))).data;
}

export async function getJob(id: string): Promise<Job> {
    return (await axios.get<Job>(`/api/plugins/${PLUGIN_ID}/jobs/${id}`)).data;
}

export async function listJobs(): Promise<Job[]> {
    return (await axios.get<Job[] | null>(`/api/plugins/${PLUGIN_ID}/jobs`)).data ?? [];
}
