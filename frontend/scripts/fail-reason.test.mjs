// node --test scripts/: a failed job's toast names the reason, not a generic
// «the agent failed» (it once read «Запуск агента…» for an expired CLI sign-in).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { jobFailReason } from '../src/lib/failReason.ts'
import ru from '../src/i18n/ru.ts'

const text = (key) => key.split('.').reduce((n, k) => n?.[k], ru)

test('agent_failed shows the CLI error', () => {
  assert.equal(jobFailReason('agent_failed', 'claude exited with 1: rate limited\nmore', text), 'claude exited with 1: rate limited')
})

test('agent_auth says to sign in again', () => {
  assert.match(jobFailReason('agent_auth', 'claude exited with 1: Failed to authenticate', text), /\/login/)
})

test('a code with its own text uses it', () => {
  assert.equal(jobFailReason('no_folder', 'no local folder is mapped', text), 'Папка проекта не привязана')
  assert.equal(jobFailReason('', 'boom', text), 'boom')
})
