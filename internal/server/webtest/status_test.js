const test = require('node:test')
const assert = require('node:assert/strict')
const approvalStatus = require('../web/status.js')

test('approval status labels expose explicit unavailable and loading states', () => {
  assert.deepEqual(approvalStatus({approvalState: 'UNAVAILABLE'}), {label: 'Approvals unavailable', kind: 'warn'})
  assert.deepEqual(approvalStatus({approvalState: 'LOADING'}), {label: 'Loading approvals', kind: 'warn'})
  assert.deepEqual(approvalStatus({approvalState: 'UNKNOWN'}), {label: 'Approvals unavailable', kind: 'warn'})
})

test('approval status distinguishes no rules, pending, and satisfied rules', () => {
  assert.deepEqual(approvalStatus({approvalState: 'NOT_REQUIRED', approverCount: 0}), {label: 'No approvals required', kind: ''})
  assert.deepEqual(approvalStatus({approvalState: 'NOT_REQUIRED', approverCount: 2}), {label: '2 approvals · none required', kind: ''})
  assert.deepEqual(approvalStatus({approvalState: 'PENDING', approvalRemaining: 2}), {label: '2 approvals remaining', kind: 'warn'})
  assert.deepEqual(approvalStatus({approvalState: 'APPROVED'}), {label: 'Approval requirements met', kind: 'ok'})
})

test('approval status normalizes malformed counts without using legacy fields', () => {
  assert.deepEqual(approvalStatus({approvalState: 'PENDING', approvalRemaining: '3.9', reviewApproved: 99, reviewTotal: 99}), {label: '3 approvals remaining', kind: 'warn'})
  assert.deepEqual(approvalStatus({approvalState: 'PENDING', approvalRemaining: -2}), {label: '0 approvals remaining', kind: 'warn'})
})
