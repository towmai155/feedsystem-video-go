import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { state, reportError } = vi.hoisted(() => ({
  state: {
    token: null as string | null,
    refreshToken: null as string | null,
    setToken: (_token: string) => {},
    clearTokens: () => {},
  },
  reportError: vi.fn(),
}))

vi.mock('../src/stores/auth', () => ({ useAuthStore: () => state }))
vi.mock('../src/utils/error-reporter', () => ({ reportError }))

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

let client: typeof import('../src/api/client')

beforeEach(async () => {
  vi.resetModules()
  vi.clearAllMocks()
  state.token = 'old-access'
  state.refreshToken = 'refresh-one'
  state.setToken = vi.fn((token: string) => { state.token = token })
  state.clearTokens = vi.fn(() => { state.token = null; state.refreshToken = null })
  vi.stubGlobal('fetch', vi.fn())
  client = await import('../src/api/client')
})

afterEach(() => { vi.unstubAllGlobals() })

describe('session-aware API requests', () => {
  it('shares one refresh for concurrent JSON and form requests and retries each once', async () => {
    const refresh = deferred<Response>()
    const started = deferred<void>()
    const fetchMock = vi.mocked(fetch).mockImplementation(async (input, init) => {
      if (String(input).endsWith('/account/refresh')) { started.resolve(); return refresh.promise }
      return (init?.headers as Record<string, string>).Authorization === 'Bearer refreshed-access'
        ? json(200, { ok: true }) : json(401, { error: 'expired' })
    })
    const first = client.postJson('/like/isLiked', {}, { authRequired: true })
    const second = client.postForm('/video/uploadCover', new FormData(), { authRequired: true })
    await started.promise
    refresh.resolve(json(200, { token: 'refreshed-access' }))
    await expect(Promise.all([first, second])).resolves.toEqual([{ ok: true }, { ok: true }])
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/account/refresh'))).toHaveLength(1)
    expect(fetchMock).toHaveBeenCalledTimes(5)
    expect(state.token).toBe('refreshed-access')
  })

  it.each(['direct', 'refresh'])('preserves credentials when %s authentication returns 503', async (failure) => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      return failure === 'direct' || String(input).endsWith('/account/refresh')
        ? json(503, { error: 'authentication temporarily unavailable' })
        : json(401, { error: 'expired' })
    })
    await expect(client.postJson('/notification/list', {})).rejects.toMatchObject({ status: 503 })
    expect(state.token).toBe('old-access')
    expect(state.refreshToken).toBe('refresh-one')
    expect(state.clearTokens).not.toHaveBeenCalled()
  })

  it('preserves credentials after a network failure during refresh', async () => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      if (String(input).endsWith('/account/refresh')) throw new TypeError('network unavailable')
      return json(401, { error: 'expired' })
    })
    await expect(client.postJson('/notification/list', {})).rejects.toThrow('network unavailable')
    expect(state.clearTokens).not.toHaveBeenCalled()
  })

  it('clears the matching session when its refresh token is rejected', async () => {
    vi.mocked(fetch).mockImplementation(async () => json(401, { error: 'revoked' }))
    await expect(client.postJson('/notification/list', {})).rejects.toMatchObject({ status: 401 })
    expect(state.token).toBeNull()
    expect(state.refreshToken).toBeNull()
    expect(state.clearTokens).toHaveBeenCalledTimes(1)
  })

  it.each(['logout', 'new-login'])('ignores a late refresh response after %s', async (change) => {
    const refresh = deferred<Response>()
    const started = deferred<void>()
    vi.mocked(fetch).mockImplementation(async (input) => {
      if (String(input).endsWith('/account/refresh')) { started.resolve(); return refresh.promise }
      return json(401, { error: 'expired' })
    })
    const pending = client.postJson('/like/like', { video_id: 1 })
    const rejected = expect(pending).rejects.toMatchObject({ status: 401 })
    await started.promise
    state.token = change === 'logout' ? null : 'new-login-access'
    state.refreshToken = change === 'logout' ? null : 'refresh-two'
    refresh.resolve(json(200, { token: 'stale-refreshed-access' }))
    await rejected
    expect(state.token).toBe(change === 'logout' ? null : 'new-login-access')
    expect(state.setToken).not.toHaveBeenCalled()
    expect(state.clearTokens).not.toHaveBeenCalled()
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('does not replay an old write or clear a newer login after a delayed 401', async () => {
    const response = deferred<Response>()
    vi.mocked(fetch).mockReturnValue(response.promise)
    const pending = client.postJson('/like/like', { video_id: 1 })
    const rejected = expect(pending).rejects.toMatchObject({ status: 401 })
    state.token = 'new-login-access'
    state.refreshToken = 'refresh-two'
    response.resolve(json(401, { error: 'revoked' }))
    await rejected
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(state.token).toBe('new-login-access')
    expect(state.clearTokens).not.toHaveBeenCalled()
  })

  it('does not refresh or clear an existing session for an incorrect login password', async () => {
    vi.mocked(fetch).mockResolvedValue(json(401, { error: 'unauthorized' }))
    await expect(client.postJson('/account/login', { username: 'tester', password: 'wrong' })).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(state.clearTokens).not.toHaveBeenCalled()
  })
})
