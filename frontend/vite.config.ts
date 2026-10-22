import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';
// Sourced from vite's re-exported Rollup compat namespace rather than the
// `rollup` package: vite 8 bundles rolldown, so `rollup` is not installed.
import type { Rollup } from 'vite';
import { readFileSync } from 'fs';
import { resolve } from 'path';

// The repo-root VERSION file is the single source of truth; main.go embeds the
// same file. build.sh passes it in as PLUGIN_VERSION because its container only
// mounts frontend/, where ../VERSION does not exist.
const pluginVersion = (
    process.env.PLUGIN_VERSION || readFileSync(resolve(import.meta.dirname, '../VERSION'), 'utf8')
).trim();

// The one name the bundle exports. The panel's loader expects exactly one
// exported PluginDefinition, and the bundle check below asserts it.
const EXPORT_NAME = 'updaterPlugin';

// Every class in plugin.css must carry this prefix, because the panel injects
// plugin CSS panel-wide.
const CSS_PREFIX = '.gu-';

// GameAP provides these as globals on window at runtime (js/plugins/loader.js),
// so they are externalized and their imports rewritten to read the globals.
const GLOBALS: Record<string, string> = {
    'vue': 'window.Vue',
    'axios': 'window.axios',
    'naive-ui': 'window.NaiveUI',
    // Re-exported by @gameap/plugin-sdk; external so it is never bundled.
    '@gameap/ui': 'window.gameapUI',
};

function globalExternalsPlugin(): Rollup.Plugin {
    return {
        name: 'global-externals',
        renderChunk(code) {
            let result = code;
            for (const [moduleId, globalVar] of Object.entries(GLOBALS)) {
                // `import { a, b as c } from 'x'` -> `const a = window.X?.a, c = window.X?.b;`
                // Optional chaining so a global missing on an older panel leaves
                // undefined values instead of breaking every plugin in /plugins.js.
                const importRegex = new RegExp(
                    `import\\s*\\{([^}]+)\\}\\s*from\\s*["']${moduleId}["'];?`,
                    'g'
                );
                result = result.replace(importRegex, (_: string, imports: string) => {
                    const assignments = imports
                        .split(',')
                        .map((i) => i.trim())
                        .filter(Boolean)
                        .map((i) => {
                            const [original, alias = original] = i.split(/\s+as\s+/).map((s) => s.trim());
                            return `${alias} = ${globalVar}?.${original}`;
                        });
                    return `const ${assignments.join(', ')};`;
                });

                const importStarRegex = new RegExp(
                    `import\\s*\\*\\s*as\\s*(\\w+)\\s*from\\s*["']${moduleId}["'];?`,
                    'g'
                );
                result = result.replace(importStarRegex, (_, name) => `const ${name} = ${globalVar};`);

                const importDefaultRegex = new RegExp(
                    `import\\s+(\\w+)\\s*from\\s*["']${moduleId}["'];?`,
                    'g'
                );
                result = result.replace(importDefaultRegex, (_, name) => `const ${name} = ${globalVar};`);

                // A bare side-effect import is what is left when every named
                // import of an external was tree-shaken. Drop it.
                result = result.replace(new RegExp(`import\\s*["']${moduleId}["'];?`, 'g'), '');
            }
            return { code: result, map: null };
        },
    };
}

function wrapInIIFEPlugin(): Rollup.Plugin {
    return {
        name: 'wrap-iife',
        generateBundle(_options, bundle) {
            for (const fileName of Object.keys(bundle)) {
                const chunk = bundle[fileName];
                if (chunk.type === 'chunk' && chunk.code) {
                    const exportMatch = chunk.code.match(/export\s*\{\s*(\w+)(?:\s+as\s+(\w+))?\s*\};?\s*$/s);
                    if (exportMatch) {
                        const [fullExport, internalName, alias] = exportMatch;
                        const exportedName = alias ?? internalName;
                        const codeWithoutExport = chunk.code.replace(fullExport, '').trim();
                        chunk.code = `const ${exportedName} = (function() {\n${codeWithoutExport}\nreturn ${internalName};\n})();\nexport { ${exportedName} };`;
                    }
                }
            }
        },
    };
}

/** Selectors and at-rules in a stylesheet that fall outside the plugin's prefix. */
function unprefixedSelectors(css: string): string[] {
    const offenders: string[] = [];
    let head = '';
    for (const ch of css.replace(/\/\*[\s\S]*?\*\//g, '')) {
        if (ch === '{') {
            const rule = head.trim();
            head = '';
            if (rule.startsWith('@')) {
                if (!/^@(media|supports|container)\b/.test(rule)) offenders.push(rule);
                continue;
            }
            for (const selector of rule.split(',')) {
                if (!selector.includes(CSS_PREFIX)) offenders.push(selector.trim());
            }
        } else if (ch === '}' || ch === ';') {
            head = '';
        } else {
            head += ch;
        }
    }
    return offenders;
}

/**
 * Fail the build when the bundle is not in the shape the panel's loader needs:
 * no surviving imports of externals, exactly one export inside an IIFE, Vue read
 * from the panel global, and CSS kept inside the plugin's prefix.
 */
function assertBundleShapePlugin(): Rollup.Plugin {
    return {
        name: 'assert-bundle-shape',
        writeBundle(options) {
            const dir = options.dir ?? resolve(import.meta.dirname, 'dist');
            const js = readFileSync(resolve(dir, 'plugin.js'), 'utf8');
            const problems: string[] = [];

            const imports = js.match(/^\s*import\b.*$/gm) ?? [];
            if (imports.length) problems.push(`plugin.js still imports: ${imports.map((l) => l.trim()).join(' | ')}`);
            const exports = (js.match(/^\s*export\b.*$/gm) ?? []).map((l) => l.trim());
            if (exports.join('\n') !== `export { ${EXPORT_NAME} };`) problems.push(`plugin.js exports: ${exports.join(' | ') || '(none)'}`);
            if (!new RegExp(`\\}\\)\\(\\);\\s*export \\{ ${EXPORT_NAME} \\};\\s*$`).test(js)) problems.push('plugin.js is not wrapped in an IIFE');
            if (!js.includes('window.Vue?.')) problems.push('plugin.js does not read Vue from the panel global');

            let css = '';
            try {
                css = readFileSync(resolve(dir, 'plugin.css'), 'utf8');
            } catch {
                problems.push('plugin.css was not emitted (src/index.ts must import src/styles.css)');
            }
            if (css.includes('@import')) problems.push('plugin.css contains @import');
            const offenders = unprefixedSelectors(css);
            if (offenders.length) problems.push(`plugin.css selectors outside ${CSS_PREFIX}: ${offenders.join(' | ')}`);

            if (problems.length) throw new Error(`Plugin bundle check failed:\n - ${problems.join('\n - ')}`);
        },
    };
}

export default defineConfig({
    root: import.meta.dirname,
    plugins: [vue()],
    define: {
        __PLUGIN_VERSION__: JSON.stringify(pluginVersion),
    },
    build: {
        lib: {
            entry: resolve(import.meta.dirname, 'src/index.ts'),
            formats: ['es'],
            fileName: () => 'plugin.js',
            // main.go embeds dist/plugin.css, so name it that here.
            cssFileName: 'plugin',
        },
        outDir: 'dist',
        emptyOutDir: true,
        rollupOptions: {
            external: Object.keys(GLOBALS),
            plugins: [globalExternalsPlugin(), wrapInIIFEPlugin(), assertBundleShapePlugin()],
        },
    },
    resolve: {
        alias: {
            '@': resolve(import.meta.dirname, 'src'),
        },
    },
});
