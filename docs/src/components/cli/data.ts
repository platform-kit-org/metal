// The pk-metal command data for the CLI reference components.
// Source: src/data/generated/cli/*.json, which metal/hack/docgen writes from the
// command tree that `pk-metal __docs` prints (metal/cmd/pk-metal/cli.go).
import { readFileSync } from 'node:fs';
import { root } from '../../../scripts/root.mjs';

const dir = `${root}/src/data/generated/cli`;

export interface Flag {
  name: string;
  shorthand?: string;
  type: string;
  default: string;
  usage: string;
}
export interface Sub {
  name: string;
  synopsis: string;
  flags: Flag[];
}
export interface Command {
  name: string;
  summary: string;
  synopsis: string;
  changes: boolean;
  needsCluster: boolean;
  exitStatus: { code: number; meaning: string }[];
  flags: Flag[];
  subcommands: Sub[];
}
export interface Index {
  commands: { name: string; summary: string; changes: boolean }[];
  exitStatus: { code: number; meaning: string }[];
}

const read = (name: string) => JSON.parse(readFileSync(`${dir}/${name}.json`, 'utf8'));

export const index: Index = read('index');

const cache = new Map<string, Command>();
export function command(name: string): Command {
  if (!index.commands.some((c) => c.name === name)) {
    throw new Error(`cli: "${name}" is not a pk-metal command (src/data/generated/cli/index.json)`);
  }
  if (!cache.has(name)) cache.set(name, read(name));
  return cache.get(name)!;
}

/**
 * The groups of the command table, by purpose. Each command is in one group.
 * CliIndex stops the build when index.json has a command that no group names.
 */
export const groups = [
  {
    id: 'inspect',
    title: 'Inspect',
    lead: 'Read the state of the machines, the BMCs and the clusters.',
    commands: ['status', 'top', 'check', 'inventory', 'version'],
  },
  {
    id: 'control',
    title: 'Approve and control',
    lead: 'Decide what the operator does with its proposals.',
    commands: ['approve', 'retry', 'abort', 'hold', 'unhold', 'rollout'],
  },
  {
    id: 'day-1',
    title: 'Day 1: first machines',
    lead: 'Start the first machines of a site without the operator, then move the operator.',
    commands: ['render', 'boot', 'bootstrap', 'relay', 'pivot'],
  },
  {
    id: 'day-2',
    title: 'Day 2: operation',
    lead: 'Ask the operator for a change to a node or to a cluster that runs.',
    commands: ['reboot', 'upgrade', 'scale', 'reinstall', 'replace', 'wipe', 'backup', 'restore'],
  },
  {
    id: 'lab',
    title: 'Lab and simulation',
    lead: 'Run simulated BMCs for tests and for the virtual lab.',
    commands: ['sim'],
  },
  {
    id: 'shell',
    title: 'Shell',
    lead: 'Make the program easier to use in your terminal.',
    commands: ['completion'],
  },
];

/** `--yes`, or `-n, --namespace` for a flag with a shorthand. */
export function flagText(f: Flag): string {
  return f.shorthand ? `-${f.shorthand}, --${f.name}` : `--${f.name}`;
}

/** The anchor of a flag row: `--yes`, or `now.--wait` for the flag of a subcommand. */
export function flagId(f: Flag, sub?: string): string {
  return sub ? `${sub}.--${f.name}` : `--${f.name}`;
}

/** The value placeholder after a flag: nothing for a bool, `<duration>` and so on for the rest. */
export function flagValue(f: Flag): string {
  return f.type === 'bool' ? '' : f.type;
}
