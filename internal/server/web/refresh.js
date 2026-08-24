(function (root, factory) {
  const timing = factory()
  if (typeof module === 'object' && module.exports) module.exports = timing
  else root.refreshTiming = timing
})(globalThis, function () {
  const refreshInterval = 300000
  const retryBaseDelay = 30000
  const retryMaxDelay = 300000
  const minimumDelay = 1000
  const refreshTimeout = 125000

  function retryDelay(failures) {
    return Math.min(retryMaxDelay, retryBaseDelay * (2 ** Math.max(0, failures - 1)))
  }

  function nextDelay(now, lastUpdated, failures, retryAt) {
    if (failures > 0) return Math.max(minimumDelay, retryAt - now)
    return Math.max(minimumDelay, refreshInterval - (now - lastUpdated))
  }

  function withTimeout(task) {
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), refreshTimeout)
    return Promise.resolve().then(() => task(controller.signal)).finally(() => clearTimeout(timer))
  }

  function isAbortError(error) {
    return error?.name === 'AbortError'
  }

  function timeoutMessage() {
    return `Refresh timed out after ${Math.round(refreshTimeout / 1000)} seconds`
  }

  return {refreshInterval, refreshTimeout, retryDelay, nextDelay, withTimeout, isAbortError, timeoutMessage}
})
