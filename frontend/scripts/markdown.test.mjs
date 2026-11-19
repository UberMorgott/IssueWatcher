// node --test scripts/: issue bodies render a safe Markdown subset as tokens, never HTML.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { inline, markdown } from '../src/lib/markdown.ts'

test('inline code, bold, italic, links', () => {
  assert.deepEqual(inline('a **b** _c_ *d* `e` [f](https://x.y/z)'), [
    { t: 'text', v: 'a ' }, { t: 'b', v: 'b' }, { t: 'text', v: ' ' }, { t: 'i', v: 'c' }, { t: 'text', v: ' ' },
    { t: 'i', v: 'd' }, { t: 'text', v: ' ' }, { t: 'code', v: 'e' }, { t: 'text', v: ' ' }, { t: 'a', v: 'f', href: 'https://x.y/z' },
  ])
})

test('HTML stays text; link targets pass through for safeUrl in the renderer', () => {
  assert.deepEqual(inline('<b>y</b> <img src=x onerror=alert(1)>'), [{ t: 'text', v: '<b>y</b> <img src=x onerror=alert(1)>' }])
  assert.deepEqual(inline('[x](javascript:alert(1))'), [{ t: 'a', v: 'x', href: 'javascript:alert(1' }, { t: 'text', v: ')' }])
})

test('snake_case and 2 * 3 * 4 are not italic', () => {
  assert.deepEqual(inline('my_var_name 2 * 3 * 4'), [{ t: 'text', v: 'my_var_name 2 * 3 * 4' }])
})

test('fences become code blocks, headings bold', () => {
  assert.deepEqual(markdown('## Steps\r\n\r\n```lua\nlocal **x** = 1\n```\n\nafter'), [
    { t: 'text', v: [{ t: 'b', v: 'Steps' }] },
    { t: 'pre', v: 'local **x** = 1' },
    { t: 'text', v: [{ t: 'text', v: 'after' }] },
  ])
})

test('unclosed fence runs to the end', () => {
  assert.deepEqual(markdown('a\n~~~\nb'), [{ t: 'text', v: [{ t: 'text', v: 'a' }] }, { t: 'pre', v: 'b' }])
})
