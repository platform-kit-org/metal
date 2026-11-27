// Loads the glossary: src/data/glossary.yaml, or the sample file while that does not exist.
//
// The site links pages to the glossary by id and by form, so a broken entry breaks links
// on pages that nobody edited. The loader checks the shape of each entry and stops the
// build with every problem it finds. Vale checks the wording of the definitions
// (mise run docs:lint:glossary); GlossaryPage checks that each `see` page exists.
import { existsSync, readFileSync } from 'node:fs';
import { z } from 'astro/zod';
import { parse } from 'yaml';

import { byScheme } from '../src/data/categories.mjs';
import { root } from './root.mjs';

const dataDir = `${root}/src/data/`;
let cache;

const id = z.string().regex(/^[a-z0-9]+(-[a-z0-9]+)*$/, 'an id is lower case words joined with "-"');
const regex = z.string().refine((s) => {
  try {
    new RegExp(s);
    return true;
  } catch {
    return false;
  }
}, 'not a regular expression');
const term = z
  .object({
    id,
    term: z.string().min(1),
    abbr: z.string().optional(),
    kind: z.enum(['noun', 'verb', 'api-kind', 'abbr', 'tool', 'phase', 'annotation']),
    domain: z.string(),
    forms: z.array(z.string()).optional(),
    guards: z.object({ not_before: regex, not_after: regex, only_after: regex }).partial().strict().optional(),
    scope: z.literal('page').optional(),
    see: z.string().regex(/^[a-z]+:[A-Za-z0-9/_.#-]+$/).refine((s) => byScheme.has(s.split(':')[0]), 'see is not a category link'),
    related: z.array(id).optional(),
    def: z.string().min(1),
  })
  .strict()
  .refine((t) => t.kind !== 'abbr' || t.abbr, 'an abbr needs its expansion in abbr');
const schema = z
  .object({
    domains: z.record(z.string(), z.object({ name: z.string(), icon: z.string() }).strict()),
    terms: z.array(term).min(1),
  })
  .strict();

/** @returns {{ domains: Record<string, {name: string, icon: string}>, terms: any[], file: string }} */
export function loadGlossary() {
  if (cache) return cache;
  const file = ['glossary.yaml', 'glossary-sample.yaml'].map((f) => dataDir + f).find((f) => existsSync(f));
  if (!file) throw new Error('glossary: no src/data/glossary.yaml');
  const raw = parse(readFileSync(file, 'utf8'));
  const parsed = schema.safeParse(raw);
  if (!parsed.success) fail(file, parsed.error.issues.map((x) => `${where(raw, x.path)}: ${x.message}`));
  const { domains, terms } = parsed.data;
  const errs = [];
  const ids = new Set();
  const forms = new Map();
  for (const t of terms) {
    if (ids.has(t.id)) errs.push(`term "${t.id}": the id is used twice`);
    ids.add(t.id);
    if (!domains[t.domain]) errs.push(`term "${t.id}": the domain "${t.domain}" is not in domains`);
    for (const f of t.forms ?? [t.term]) {
      if (forms.has(f)) errs.push(`term "${t.id}": the form "${f}" is also a form of "${forms.get(f)}"`);
      forms.set(f, t.id);
    }
  }
  for (const t of terms) for (const r of t.related ?? []) if (!ids.has(r)) errs.push(`term "${t.id}": related "${r}" is no term id`);
  if (errs.length) fail(file, errs);
  cache = { domains, terms, file };
  return cache;
}

/** A schema path ['terms', 3, 'kind'] reads as: term "bmc": kind. */
function where(raw, [top, i, ...rest]) {
  const head = top === 'terms' && typeof i === 'number' ? [`term "${raw.terms[i]?.id}"`] : [top, i];
  return [...head, ...rest].filter((p) => p !== undefined).join(': ');
}

function fail(file, errs) {
  throw new Error(`glossary: ${file}:\n  ${errs.join('\n  ')}`);
}
