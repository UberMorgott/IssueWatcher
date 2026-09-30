// node --test scripts/: every UI message compiles under vue-i18n's syntax. A
// literal @ | { } in a message is syntax there ("@{author}" is a broken linked
// message), and a message that fails to compile throws while the page renders:
// the whole view goes blank. Such characters are written as {'@'} etc.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { baseCompile } from '@intlify/message-compiler'
import en from '../src/i18n/en.ts'
import ru from '../src/i18n/ru.ts'

function* leaves(node, path) {
  if (typeof node === 'string') {
    yield [path, node]
    return
  }
  for (const [k, v] of Object.entries(node)) yield* leaves(v, path ? `${path}.${k}` : k)
}

for (const [lang, messages] of [['en', en], ['ru', ru]]) {
  test(`${lang} messages compile`, () => {
    const broken = []
    for (const [key, msg] of leaves(messages, '')) {
      baseCompile(msg, { onError: (e) => broken.push(`${key}: ${e.message}`) })
    }
    assert.deepEqual(broken, [])
  })

  // Shown for items of every platform (a Steam reply draft said «аккаунта GitHub»).
  test(`${lang} platform-neutral messages name {platform}`, () => {
    for (const msg of [messages.job.draftText, messages.item.openComment]) {
      assert.match(msg, /\{platform\}/)
      assert.doesNotMatch(msg, /GitHub|Steam|Nexus|CurseForge|Factorio/)
    }
  })
}
