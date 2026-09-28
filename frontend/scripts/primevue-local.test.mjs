// node --test scripts/: the PrimeVue license patch is idempotent and refuses
// unknown upstream sources before writing anything.
import assert from 'node:assert/strict'
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { patchPrimeVue } from './primevue-local.mjs'

const web = fileURLToPath(new URL('../', import.meta.url))
const files = ['primevue/package.json', '@primevue/core/package.json', '@primevue/core/config/index.mjs', '@primevue/core/basecomponent/index.mjs']

test('patch is idempotent and fails loudly on drift', () => {
  const root = mkdtempSync(join(tmpdir(), 'iw-primevue-'))
  const target = join(root, 'node_modules')
  const read = () => files.map((f) => readFileSync(join(target, f), 'utf8'))
  try {
    for (const f of files) {
      mkdirSync(dirname(join(target, f)), { recursive: true })
      cpSync(join(web, 'node_modules', f), join(target, f))
    }
    patchPrimeVue(root)
    const first = read()
    assert.ok(!first[2].includes('verifyLicense'))
    assert.ok(!first[3].includes('showInvalidLicenseBanner'))
    assert.equal(patchPrimeVue(root), 0)
    assert.deepEqual(read(), first)

    const component = join(target, '@primevue/core/basecomponent/index.mjs')
    writeFileSync(component, first[3] + '\n// unexpected upstream change\n')
    const before = read()
    assert.throws(() => patchPrimeVue(root), /unexpected source basecomponent\/index\.mjs/)
    assert.deepEqual(read(), before)

    const manifest = join(target, '@primevue/core/package.json')
    writeFileSync(manifest, JSON.stringify({ ...JSON.parse(before[1]), version: '5.0.2' }))
    assert.throws(() => patchPrimeVue(root), /requires @primevue\/core@5\.0\.1; found 5\.0\.2/)
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})
