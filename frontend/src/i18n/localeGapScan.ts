import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  LOCALE_BUNDLES,
  collectI18nUsageFromSources,
  collectLocaleKeys,
  collectReferencedLocaleKeys,
  collectStaticI18nKeysFromSources,
} from './localeKeyAudit.ts'

const SOURCE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const EXT = /\.(vue|ts|js|mjs)$/
const SKIP = /\/i18n\/locales\/|localeGapScan\.ts$|\.test\.(ts|mjs|js)$/

function walk(dir: string, out: string[] = []): string[] {
  for (const ent of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, ent.name)
    if (ent.isDirectory()) walk(p, out)
    else if (EXT.test(ent.name) && !SKIP.test(p)) out.push(p)
  }
  return out
}

const TEMPLATE_RE = /(?:\$t|(?<![.\w])t|i18n\.global\.t|globalSettingsText)\(\s*`([^`]+)`/g
const CONCAT_RE = /(?:\$t|(?<![.\w])t)\(\s*['"]([a-zA-Z][\w.]*\.)['"]\s*\+/g
const TM_ROOT_RE = /tm\(\s*['"]([^'"]+)['"]\s*\)/g

const templatePrefixes = new Map<string, Set<string>>()
const concatPrefixes = new Map<string, Set<string>>()
const tmRoots = new Map<string, Set<string>>()

for (const file of walk(SOURCE_ROOT)) {
  const rel = file.replace(SOURCE_ROOT + '/', '')
  const content = readFileSync(file, 'utf8')
  let match: RegExpExecArray | null

  TEMPLATE_RE.lastIndex = 0
  while ((match = TEMPLATE_RE.exec(content))) {
    const tpl = match[1]
    const idx = tpl.indexOf('${')
    const prefix = idx === -1 ? tpl : tpl.slice(0, idx)
    const bucket = templatePrefixes.get(prefix) ?? new Set()
    bucket.add(rel)
    templatePrefixes.set(prefix, bucket)
  }

  CONCAT_RE.lastIndex = 0
  while ((match = CONCAT_RE.exec(content))) {
    const prefix = match[1]
    const bucket = concatPrefixes.get(prefix) ?? new Set()
    bucket.add(rel)
    concatPrefixes.set(prefix, bucket)
  }

  TM_ROOT_RE.lastIndex = 0
  while ((match = TM_ROOT_RE.exec(content))) {
    const root = match[1]
    const bucket = tmRoots.get(root) ?? new Set()
    bucket.add(rel)
    tmRoots.set(root, bucket)
  }
}

const usage = collectI18nUsageFromSources()
const enKeys = collectLocaleKeys(LOCALE_BUNDLES['en-US'])
const referenced = collectReferencedLocaleKeys(LOCALE_BUNDLES['en-US'], usage)

function prefixMatchesConfigured(prefix: string): boolean {
  const normalized = prefix.endsWith('.') ? prefix : `${prefix}.`
  for (const configured of usage.prefixes) {
    if (normalized.startsWith(configured) || configured.startsWith(normalized)) return true
  }
  return false
}

function keysUnderPrefix(prefix: string): string[] {
  const normalized = prefix.endsWith('.') ? prefix : `${prefix}.`
  return [...enKeys].filter((key) => key.startsWith(normalized))
}

function missingUnderPrefix(prefix: string): string[] {
  return keysUnderPrefix(prefix).filter((key) => !referenced.has(key))
}

type Gap = {
  kind: 'template' | 'concat' | 'tm'
  prefix: string
  files: string[]
  localeKeys: number
  missingKeys: string[]
  configured: boolean
}

const gaps: Gap[] = []

for (const [prefix, files] of templatePrefixes) {
  if (!prefix.includes('.') && !prefix.includes('${')) continue
  const missing = missingUnderPrefix(prefix)
  const configured = prefixMatchesConfigured(prefix)
  if (!configured || missing.length > 0) {
    gaps.push({
      kind: 'template',
      prefix,
      files: [...files],
      localeKeys: keysUnderPrefix(prefix).length,
      missingKeys: missing.slice(0, 5),
      configured,
    })
  }
}

for (const [prefix, files] of concatPrefixes) {
  const missing = missingUnderPrefix(prefix)
  const configured = prefixMatchesConfigured(prefix)
  if (!configured || missing.length > 0) {
    gaps.push({
      kind: 'concat',
      prefix,
      files: [...files],
      localeKeys: keysUnderPrefix(prefix).length,
      missingKeys: missing.slice(0, 5),
      configured,
    })
  }
}

for (const [root, files] of tmRoots) {
  const missing = missingUnderPrefix(root)
  const configured = prefixMatchesConfigured(root)
  if (!configured || missing.length > 0) {
    gaps.push({
      kind: 'tm',
      prefix: root,
      files: [...files],
      localeKeys: keysUnderPrefix(root).length,
      missingKeys: missing.slice(0, 5),
      configured,
    })
  }
}

console.log('=== Main app: dynamic prefix gaps ===')
console.log(`Template literal prefixes: ${templatePrefixes.size}`)
console.log(`String concat prefixes: ${concatPrefixes.size}`)
console.log(`tm() roots: ${tmRoots.size}`)
console.log(`Potential gaps: ${gaps.length}`)
for (const gap of gaps.sort((a, b) => a.prefix.localeCompare(b.prefix))) {
  console.log(`\n[${gap.kind}] ${gap.prefix}`)
  console.log(`  configured: ${gap.configured}, locale keys: ${gap.localeKeys}, missing referenced: ${gap.missingKeys.length}`)
  if (gap.missingKeys.length) console.log(`  sample missing: ${gap.missingKeys.join(', ')}`)
  console.log(`  files: ${gap.files.slice(0, 3).join(', ')}${gap.files.length > 3 ? '...' : ''}`)
}

console.log('\n=== Main locale uncovered keys (in bundle, not referenced) ===')
const uncovered = [...enKeys].filter((key) => !referenced.has(key))
console.log(`Count: ${uncovered.length}`)

console.log('\n=== Indirect i18n keys (computed / inline ternary) ===')
const staticKeys = collectStaticI18nKeysFromSources()
const indirectCandidates = [
  'knowledgeList.sections.tenantOthers',
  'knowledgeList.sections.tenantReadonly',
  'agent.sections.tenantOthers',
  'agent.sections.tenantReadonly',
  'modelSettings.builtinModels.descriptionAdmin',
  'modelSettings.builtinModels.description',
]
for (const key of indirectCandidates) {
  console.log(
    `${key}: tracked=${staticKeys.has(key)} locale=${enKeys.has(key)} referenced=${referenced.has(key)}`,
  )
}
