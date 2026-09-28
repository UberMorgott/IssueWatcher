// Removes PrimeVue 5's PrimeUI license check (console warning + "invalid
// license" banner) from node_modules after install, so no license key has to
// ship inside the public exe. Adapted from E:\DEV\Pult\web\scripts.
//
// Safe by construction: the target files must match the exact upstream bytes
// (SHA-256) of the pinned PrimeVue version, or already be patched (idempotent).
// Any other version or content fails loudly before anything is written; review
// the upstream change, then update the hashes and replacements below.
// Local runtime customization only. Upstream copyright and license terms remain.
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { argv } from 'node:process'
import { fileURLToPath, pathToFileURL, URL } from 'node:url'

export const PRIMEVUE_VERSION = '5.0.1'

const bannerImport = "import { showInvalidLicenseBanner } from '@primevue/core/license/licenseBanner';\n"
const patches = [
  {
    file: 'config/index.mjs',
    original: '7669fa4e61336b88b9c55680371f1d6a377a9119c9aad647a5ec2a59242ef78a',
    patched: '154465f2a4ae4f21a588637693e4709ba0f03373405bd888ce05f6fd1ad787c0',
    replacements: [
      ["import { registerLicense, verifyLicense } from '@primeui/license-manager';\n", ''],
      [bannerImport, ''],
      ['inject, ref, readonly, reactive, watch', 'inject, ref, reactive, watch'],
      ["var RELEASE_DATE = '2026-08-13';\n", ''],
      ['  license: null,\n', ''],
      [
        `  var _verified = ref(null);
  if (options.license) {
    registerLicense({
      primeui: options.license
    });
  }
  verifyLicense('primeui', {
    releaseDate: RELEASE_DATE
  }).then(function (result) {
    _verified.value = result.valid;
    if (!result.valid) {
      // eslint-disable-next-line no-console
      console.warn("[PrimeUI] ".concat(result.message));
      showInvalidLicenseBanner();
    }
  });
`,
        '',
      ],
      ['    config: reactive(options),\n    verified: readonly(_verified)', '    config: reactive(options)'],
    ],
  },
  {
    file: 'basecomponent/index.mjs',
    original: '5f886d074a98a45524834fd90fc3c7c0f56ee99a4c9b4c2e9f9f99d4fe3e7a47',
    patched: '862ac2a4b5c8e5fdbaff0a1bdb8d9a54659eae8e3e1f7ce042f60841346df530',
    replacements: [
      [bannerImport, ''],
      ['    var _this$$primevue$verif;\n', ''],
      [
        `    if (!this.$primevue || ((_this$$primevue$verif = this.$primevue.verified) === null || _this$$primevue$verif === void 0 ? void 0 : _this$$primevue$verif.value) === false) {
      showInvalidLicenseBanner();
    }
`,
        '',
      ],
    ],
  },
]

const hash = (source) => createHash('sha256').update(source).digest('hex')

/** Patches root/node_modules/@primevue/core; returns the number of files written. */
export function patchPrimeVue(root) {
  const modules = join(root, 'node_modules')
  for (const name of ['primevue', '@primevue/core']) {
    const { version } = JSON.parse(readFileSync(join(modules, name, 'package.json'), 'utf8'))
    if (version !== PRIMEVUE_VERSION) {
      throw new Error(
        `primevue-local: requires ${name}@${PRIMEVUE_VERSION}; found ${version}. Review the upstream license check and update scripts/primevue-local.mjs.`,
      )
    }
  }
  // Validate every file before writing anything; already patched bytes pass.
  const writes = patches.map((patch) => {
    const path = join(modules, '@primevue/core', patch.file)
    const source = readFileSync(path, 'utf8')
    if (hash(source) === patch.patched) return null
    if (hash(source) !== patch.original) {
      throw new Error(`primevue-local: unexpected source ${patch.file}; refusing to patch. Review upstream changes or run npm ci.`)
    }
    let result = source
    for (const [before, after] of patch.replacements) {
      if (result.split(before).length !== 2) throw new Error(`primevue-local: patch target not found in ${patch.file}`)
      result = result.replace(before, () => after)
    }
    if (hash(result) !== patch.patched) throw new Error(`primevue-local: patched hash mismatch in ${patch.file}`)
    return { path, result }
  })
  let n = 0
  for (const w of writes) {
    if (!w) continue
    writeFileSync(w.path, w.result)
    n++
  }
  return n
}

if (argv[1] && pathToFileURL(resolve(argv[1])).href === import.meta.url) {
  const n = patchPrimeVue(fileURLToPath(new URL('../', import.meta.url)))
  console.log(n ? `primevue-local: license check removed (${n} files)` : 'primevue-local: already patched')
}
