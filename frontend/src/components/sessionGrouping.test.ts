import assert from 'node:assert/strict'
import test from 'node:test'

import {
  API_EXTERNAL_USER_SESSION_OWNER_PREFIX,
  classifyDateBucket,
  groupSessionsByDate,
  resolveSessionOrigin,
} from './sessionGrouping.ts'

test('resolveSessionOrigin distinguishes web and API sessions', () => {
  assert.deepEqual(resolveSessionOrigin({ id: '1' }), { kind: 'web' })
  assert.deepEqual(
    resolveSessionOrigin({ id: '4', user_id: 'api_tenant_key:1:10' }),
    { kind: 'api' },
  )
  assert.deepEqual(
    resolveSessionOrigin({ id: '5', user_id: `${API_EXTERNAL_USER_SESSION_OWNER_PREFIX}1:alice` }),
    { kind: 'api' },
  )
})

test('groupSessionsByDate keeps pinned sessions in their own bucket', () => {
  const now = new Date().toISOString()
  const groups = groupSessionsByDate(
    [
      { id: 'p', is_pinned: true, updated_at: now },
      { id: 't', updated_at: now },
    ],
    {
      pinned: 'Pinned',
      today: 'Today',
      yesterday: 'Yesterday',
      last7Days: '7d',
      last30Days: '30d',
      lastYear: 'Year',
      earlier: 'Earlier',
    },
    () => 'today',
  )
  assert.deepEqual(
    groups.map((g) => g.key),
    ['pinned', 'today'],
  )
})

test('classifyDateBucket buckets recent sessions as today', () => {
  assert.equal(classifyDateBucket(new Date().toISOString()), 'today')
})
