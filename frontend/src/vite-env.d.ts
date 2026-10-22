/// <reference types="vite/client" />

/**
 * Injected by vite.config.ts from the repo-root VERSION file, which main.go
 * embeds too, so the version is declared once.
 */
declare const __PLUGIN_VERSION__: string;

/**
 * Globals the panel puts on window for plugins. All optional: an older panel
 * can lack any of them, so every call site uses `?.`.
 */
interface Window {
    /** The panel's naive-ui module; absent before GameAP 4.4.0. */
    NaiveUI?: unknown;
    /** naive-ui's useDialog(), set by the panel's ContentView. */
    $dialog?: {
        warning(options: import('naive-ui').DialogOptions): unknown;
    };
    /** naive-ui's useMessage(). */
    $message?: {
        success(content: string): unknown;
        error(content: string): unknown;
    };
}
