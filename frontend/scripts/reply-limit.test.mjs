// node --test scripts/: the reply editors use the item's platform limit, not
// GitHub's 65536 (a 1028-character Steam draft once failed on «Отправить»).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { DEFAULT_MAX_REPLY, fallbackCaps, replyLength, replyLimit, replyTooLong } from '../src/lib/platforms.ts'

test('Steam accepts fewer than 1000 characters, GitHub 65536', () => {
  assert.equal(replyLimit(fallbackCaps('steam')), 999)
  assert.equal(replyLimit(fallbackCaps('github')), DEFAULT_MAX_REPLY)
  assert.equal(replyLimit({ maxReply: 0 }), 65536)
  assert.equal(replyLimit({ maxReply: 500 }), 500)
})

test('a reply over the limit is too long; surrounding space does not count', () => {
  assert.equal(replyTooLong('x'.repeat(999), 999), false)
  assert.equal(replyTooLong('  ' + 'x'.repeat(999) + '\n', 999), false)
  assert.equal(replyTooLong('x'.repeat(1028), 999), true)
})

test('length counts characters, not UTF-16 units', () => {
  assert.equal(replyLength('привет'), 6)
  assert.equal(replyLength('👍'), 1)
})
