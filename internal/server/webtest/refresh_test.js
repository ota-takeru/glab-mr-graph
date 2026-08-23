const test = require('node:test')
const assert = require('node:assert/strict')
const {refreshInterval, refreshTimeout, retryDelay, nextDelay, withTimeout, isAbortError, timeoutMessage} = require('../web/refresh.js')

test('refresh requests have a browser timeout slightly longer than the server refresh budget', () => {
  assert.equal(refreshTimeout, 125000)
  assert.equal(typeof withTimeout, 'function')
  assert.equal(timeoutMessage(), 'Refresh timed out after 125 seconds')
  assert.equal(isAbortError({name: 'AbortError'}), true)
  assert.equal(isAbortError(new Error('other')), false)
})

test('withTimeout aborts the work signal when its timer fires', async () => {
  const originalSetTimeout = global.setTimeout
  const originalClearTimeout = global.clearTimeout
  global.setTimeout = callback => {
    callback()
    return 1
  }
  global.clearTimeout = () => {}
  try {
    await assert.rejects(
      withTimeout(signal => {
        if (signal.aborted) {
          const error = new Error('aborted')
          error.name = 'AbortError'
          return Promise.reject(error)
        }
        return Promise.resolve()
      }),
      error => isAbortError(error),
    )
  } finally {
    global.setTimeout = originalSetTimeout
    global.clearTimeout = originalClearTimeout
  }
})

test('successful refreshes remain five minutes apart', () => {
  assert.equal(nextDelay(1000, 1000, 0, 0), refreshInterval)
  assert.equal(nextDelay(61000, 1000, 0, 0), 240000)
})

test('failed refreshes use capped exponential backoff', () => {
  assert.deepEqual(
    [1, 2, 3, 4, 5, 6].map(retryDelay),
    [30000, 60000, 120000, 240000, 300000, 300000],
  )
})

test('failure delay does not depend on the last successful refresh', () => {
  const now = 600000
  const retryAt = now + retryDelay(1)
  assert.equal(nextDelay(now, 0, 1, retryAt), 30000)
  assert.equal(nextDelay(now + 29000, 0, 1, retryAt), 1000)
})
