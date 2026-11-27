// Checks the links in the built site (mise run docs:links).
//
// Fails when
//   - a link in the page content points to a page or an anchor that is not in dist/
//     (Markdown links, category and glossary links, and the links of components),
//   - a link into reference/ has no category (write `[](kind:Host)`, not a path),
//   - a reference page has no `xref` frontmatter,
//   - an id of a page's `anchors` frontmatter has no anchor on that page,
//   - a page with an id has no category link from another page (an orphan),
//   - a back-link rule fails: a kind page links no controller, a controller page links
//     no kind, a command page links no how-to, runbook or tutorial.
// Usage: node scripts/check-xrefs.mjs [dist]
import { readFileSync } from 'node:fs';
import { base } from './site.mjs';
import { anchorsOf, loadPages } from './registry.mjs';
import { builtPages } from './root.mjs';

const { dist, files } = builtPages('check-xrefs');

/**
 * The content of a page: from the Markdown container to the footer of the page. A
 * component can have a <footer> too (KeyList), so the footer of the page is the last one.
 */
function contentOf(html) {
  const start = html.indexOf('class="sl-markdown-content"');
  return start < 0 ? '' : html.slice(start, html.lastIndexOf('<footer'));
}

const ids = new Map();
const pages = files.map((file) => {
  const html = readFileSync(file, 'utf8');
  const path = `${base}/${file.slice(dist.length + 1).replace(/index\.html$/, '')}`;
  ids.set(path, new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])));
  return { path, content: contentOf(html) };
});

const errors = [];
let checked = 0;
/** Category links in page content: target path -> pages that link it; page -> categories it links. */
const inbound = new Map();
const outCats = new Map();
for (const { path, content } of pages) {
  const cats = new Set();
  for (const m of content.matchAll(/<a\s([^>]*)>/g)) {
    const attrs = m[1];
    const href = /href="([^"]*)"/.exec(attrs)?.[1];
    if (!href || !(href.startsWith('#') || href.startsWith(`${base}/`))) continue;
    checked++;
    const [target, hash] = (href.startsWith('#') ? path + href : href).split('#');
    const t = ids.get(target);
    if (!t) errors.push(`${path}: ${href}: no page`);
    else if (hash && !t.has(decodeURIComponent(hash))) errors.push(`${path}: ${href}: no anchor "${hash}"`);
    const cat = /data-cat="([^"]*)"/.exec(attrs)?.[1];
    if (cat) {
      cats.add(cat);
      if (target !== path) {
        if (!inbound.has(target)) inbound.set(target, new Set());
        inbound.get(target).add(path);
      }
    } else if (href.startsWith(`${base}/reference/`) && !/class="[^"]*(\bgl\b|f-anchor|box-g|pk-)/.test(attrs)) {
      errors.push(`${path}: ${href}: a link into the reference needs a category, for example [](kind:Host)`);
    }
  }
  outCats.set(path, cats);
}

const backlinks = {
  kind: { needs: ['ctrl'], what: 'a kind page must link its controller' },
  controller: { needs: ['kind'], what: 'a controller page must link its kinds' },
  cli: { needs: ['howto', 'rb', 'tut'], what: 'a command page must link a how-to, a runbook or a tutorial' },
};
const orphans = [];
for (const p of loadPages()) {
  if (p.slug.startsWith('reference/') && !p.data.xref) errors.push(`${p.file}: a reference page needs xref frontmatter`);
  const path = `${base}/${p.slug}`;
  const t = ids.get(path);
  for (const [cat, id, anchor] of anchorsOf(p)) {
    if (t && !t.has(anchor)) errors.push(`${p.file}: anchors ${cat}:${id}: no anchor "${anchor}" on the page`);
  }
  if (!p.data.xref || p.data.type === 'landing') continue;
  if (!inbound.get(path)?.size) orphans.push(path);
  const rule = backlinks[p.data.type];
  const isIndex = p.data.xref.id === 'index' || p.data.xref.id === 'global-flags';
  if (rule && !isIndex && !rule.needs.some((c) => outCats.get(path)?.has(c))) errors.push(`${path}: ${rule.what}`);
}
for (const o of orphans) errors.push(`${o}: no other page links here (orphan); link it with its category id`);

for (const e of errors) console.error(`check-xrefs: ${e}`);
console.log(`check-xrefs: ${checked} links checked, ${errors.length} errors`);
process.exit(errors.length ? 1 : 0);
