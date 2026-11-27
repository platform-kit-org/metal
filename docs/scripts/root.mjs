// The docs project folder. Code from scripts/ also runs bundled (from dist/.prerender),
// where import.meta.url points elsewhere, so look for astro.config.mjs.
import { existsSync, readdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

function find() {
  const here = fileURLToPath(new URL('../', import.meta.url));
  for (let d of [here, process.cwd()]) {
    for (; d !== dirname(d); d = dirname(d)) {
      if (existsSync(join(d, 'astro.config.mjs')) && existsSync(join(d, 'src/content/docs'))) return d;
    }
  }
  throw new Error('docs: no astro.config.mjs above the current folder');
}

/** Absolute path of the docs folder, without a trailing slash. */
export const root = find();

/**
 * The HTML files of the built site, for the checks: the folder in the first argument,
 * else dist/. Stops the script when the folder does not exist.
 */
export function builtPages(script) {
  const dist = process.argv[2] ?? join(root, 'dist');
  if (!existsSync(dist)) {
    console.error(`${script}: ${dist} does not exist. Run mise run docs:build first.`);
    process.exit(1);
  }
  const walk = (d) => readdirSync(d).flatMap((n) => {
    const p = join(d, n);
    return statSync(p).isDirectory() ? walk(p) : n.endsWith('.html') ? [p] : [];
  });
  return { dist, files: walk(dist) };
}
