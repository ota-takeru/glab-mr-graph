(function (root, factory) {
  const status = factory()
  if (typeof module === 'object' && module.exports) module.exports = status
  else root.approvalStatus = status
})(globalThis, function () {
  const states = Object.freeze({
    UNKNOWN: 'UNKNOWN',
    UNAVAILABLE: 'UNAVAILABLE',
    LOADING: 'LOADING',
    NOT_REQUIRED: 'NOT_REQUIRED',
    PENDING: 'PENDING',
    APPROVED: 'APPROVED',
  })

  function count(value) {
    const parsed = Number(value)
    return Number.isFinite(parsed) ? Math.max(0, Math.trunc(parsed)) : 0
  }

  function approvalStatus(pr) {
    const state = typeof pr?.approvalState === 'string' ? pr.approvalState.toUpperCase() : states.UNKNOWN
    const approvers = count(pr?.approverCount)
    const remaining = count(pr?.approvalRemaining)
    switch (state) {
      case states.LOADING:
        return {label: 'Loading approvals', kind: 'warn'}
      case states.NOT_REQUIRED:
        return {label: approvers === 0 ? 'No approvals required' : `${approvers} approvals · none required`, kind: ''}
      case states.PENDING:
        return {label: `${remaining} approvals remaining`, kind: 'warn'}
      case states.APPROVED:
        return {label: 'Approval requirements met', kind: 'ok'}
      case states.UNAVAILABLE:
      case states.UNKNOWN:
      default:
        return {label: 'Approvals unavailable', kind: 'warn'}
    }
  }

  approvalStatus.states = states
  return approvalStatus
})
