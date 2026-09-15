import test from 'node:test'
import assert from 'node:assert/strict'

import {
  getPeopleTaskStatusMeta,
  getPersonAvatarFallback,
  getPersonCategoryLabel,
  getMergeSuggestionEmptyResultMessage,
  getMergeSuggestionFailureStateLabel,
  getMergeSuggestionRetryFeedback,
  getMergeSuggestionTaskStatusMeta,
  sortPeopleForDisplay,
} from '../src/views/People/peopleHelpers.ts'

test('getPersonCategoryLabel 映射固定人物类别文案', () => {
  assert.equal(getPersonCategoryLabel('family'), '家人')
  assert.equal(getPersonCategoryLabel('friend'), '亲友')
  assert.equal(getPersonCategoryLabel('acquaintance'), '熟人')
  assert.equal(getPersonCategoryLabel('stranger'), '路人')
  assert.equal(getPersonCategoryLabel('unknown'), '未知')
})

test('sortPeopleForDisplay 按 家人 > 亲友 > 熟人 > 路人 排序', () => {
  const people = sortPeopleForDisplay([
    { id: 4, category: 'stranger', photo_count: 9, face_count: 9 },
    { id: 2, category: 'friend', photo_count: 2, face_count: 3 },
    { id: 3, category: 'acquaintance', photo_count: 4, face_count: 4 },
    { id: 1, category: 'family', photo_count: 1, face_count: 2 },
  ])

  assert.deepEqual(people.map(person => person.id), [1, 2, 3, 4])
})

test('getPeopleTaskStatusMeta 返回任务状态胶囊文案', () => {
  assert.deepEqual(getPeopleTaskStatusMeta('running'), { label: '运行中', type: 'warning' })
  assert.deepEqual(getPeopleTaskStatusMeta('stopping'), { label: '停止中', type: 'warning' })
  assert.deepEqual(getPeopleTaskStatusMeta('failed'), { label: '失败', type: 'danger' })
  assert.deepEqual(getPeopleTaskStatusMeta(undefined), { label: '未运行', type: 'info' })
})

test('getPersonAvatarFallback 优先使用姓名首字，其次使用类别占位字', () => {
  assert.equal(getPersonAvatarFallback({ name: 'Alice', category: 'family' }), 'A')
  assert.equal(getPersonAvatarFallback({ name: ' 小王 ', category: 'friend' }), '小')
  assert.equal(getPersonAvatarFallback({ category: 'family' }), '家')
  assert.equal(getPersonAvatarFallback({ category: 'friend' }), '友')
  assert.equal(getPersonAvatarFallback({ category: 'acquaintance' }), '熟')
  assert.equal(getPersonAvatarFallback({ category: 'stranger' }), '路')
})

test('getMergeSuggestionTaskStatusMeta 暂停优先于 partial，idle+exhausted 显示部分完成', () => {
  assert.deepEqual(
    getMergeSuggestionTaskStatusMeta({ status: 'paused', partial: true }),
    { label: '已暂停（有未解决问题）', type: 'info' },
  )
  assert.deepEqual(
    getMergeSuggestionTaskStatusMeta({ status: 'idle', partial: true }),
    { label: '部分完成', type: 'warning' },
  )
  assert.deepEqual(
    getMergeSuggestionTaskStatusMeta({ status: 'idle', partial: false }),
    { label: '等待巡检', type: 'info' },
  )
})

test('getMergeSuggestionFailureStateLabel 区分等待恢复/耗尽/证据/空结果', () => {
  assert.equal(getMergeSuggestionFailureStateLabel('retry_wait'), '等待画像恢复')
  assert.equal(getMergeSuggestionFailureStateLabel('exhausted'), '重试已耗尽')
  assert.equal(getMergeSuggestionFailureStateLabel('waiting_evidence'), '等待更多有效证据')
  assert.equal(getMergeSuggestionFailureStateLabel('resolved', 'zero_candidate'), '巡检完成，无达标候选')
})

test('getMergeSuggestionEmptyResultMessage 仅在无未解决时返回正常空结果文案', () => {
  assert.equal(getMergeSuggestionEmptyResultMessage({ partial: true, unresolved_target_count: 1 }), '')
  assert.equal(
    getMergeSuggestionEmptyResultMessage({ partial: false, unresolved_target_count: 0, current_message: '本轮巡检完成' }),
    '巡检完成，无达标候选',
  )
})

test('getMergeSuggestionRetryFeedback 区分已安排、重复安排、等待证据与失败跳过', () => {
  assert.deepEqual(
    getMergeSuggestionRetryFeedback({ accepted_target_ids: [99], skipped: [] }),
    { kind: 'success', message: '已安排有界重验' },
  )
  assert.deepEqual(
    getMergeSuggestionRetryFeedback({ accepted_target_ids: [], skipped: [{ reason: 'already_scheduled' }] }),
    { kind: 'info', message: '已有人工重试在队列中，未重复安排' },
  )
  assert.deepEqual(
    getMergeSuggestionRetryFeedback({ accepted_target_ids: [], skipped: [{ reason: 'waiting_evidence' }] }),
    { kind: 'warning', message: '目标仍在等待更多有效证据，未安排技术重试' },
  )
  assert.deepEqual(
    getMergeSuggestionRetryFeedback({ accepted_target_ids: [], skipped: [{ reason: 'not_found' }] }),
    { kind: 'warning', message: '未安排重试：not_found' },
  )
})
