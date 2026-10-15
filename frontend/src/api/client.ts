import { useAuthStore } from '../stores/auth'
import { reportError } from '../utils/error-reporter'

export class ApiError extends Error {
  status: number
  payload?: unknown

  constructor(message: string, status: number, payload?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

type ApiErrorBody = { error?: string }
type AuthSnapshot = { token: string | null; refreshToken: string | null }
type RequestOptions = { authRequired?: boolean }

const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'
const credentialEndpoints = new Set(['/account/login', '/account/register', '/account/changePassword'])
const refreshRequests = new Map<string, Promise<string | null>>()

function snapshotAuth(): AuthSnapshot {
  const auth = useAuthStore()
  return { token: auth.token, refreshToken: auth.refreshToken }
}

function isCurrent(snapshot: AuthSnapshot): boolean {
  const auth = useAuthStore()
  return auth.token === snapshot.token && auth.refreshToken === snapshot.refreshToken
}

function tryRefresh(): Promise<string | null> {
  const snapshot = snapshotAuth()
  const refreshToken = snapshot.refreshToken
  if (!refreshToken) return Promise.resolve(null)
  const pending = refreshRequests.get(refreshToken)
  if (pending) return pending

  const request = Promise.resolve().then(async () => {
    const res = await fetch(`${API_BASE}/account/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: refreshToken }),
    })
    const data = await handleResponse<{ token: string }>(res, '/account/refresh', snapshot)
    // A response from a previous session must not restore a logged-out user or
    // overwrite credentials established by a newer login/rename.
    if (!isCurrent(snapshot)) return null
    useAuthStore().setToken(data.token)
    return data.token
  }).finally(() => {
    if (refreshRequests.get(refreshToken) === request) refreshRequests.delete(refreshToken)
  })
  refreshRequests.set(refreshToken, request)
  return request
}

async function send<T>(path: string, body: BodyInit, headers: Record<string, string>, options?: RequestOptions): Promise<T> {
  const snapshot = snapshotAuth()
  if (options?.authRequired && !snapshot.token) {
    throw new ApiError('需要先登录（缺少 token）', 401)
  }
  if (snapshot.token && !credentialEndpoints.has(path) && path !== '/account/refresh') {
    headers.Authorization = `Bearer ${snapshot.token}`
  }
  const res = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body })

  if (res.status === 401 && path !== '/account/refresh' && !credentialEndpoints.has(path) &&
      snapshot.refreshToken && useAuthStore().refreshToken === snapshot.refreshToken) {
    const auth = useAuthStore()
    const newToken = auth.token !== snapshot.token ? auth.token : await tryRefresh()
    if (newToken && useAuthStore().refreshToken === snapshot.refreshToken) {
      const retrySnapshot = snapshotAuth()
      headers.Authorization = `Bearer ${newToken}`
      const retryRes = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body })
      return handleResponse<T>(retryRes, path, retrySnapshot)
    }
  }
  return handleResponse<T>(res, path, snapshot)
}

export function postJson<T>(path: string, body: unknown, options?: RequestOptions): Promise<T> {
  return send<T>(path, JSON.stringify(body ?? {}), { 'Content-Type': 'application/json' }, options)
}

export function postForm<T>(path: string, body: FormData, options?: RequestOptions): Promise<T> {
  return send<T>(path, body, {}, options)
}

async function handleResponse<T>(res: Response, path: string, snapshot: AuthSnapshot): Promise<T> {
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try { data = JSON.parse(text) } catch { data = text }
  }
  if (!res.ok) {
    if (res.status === 401 && !credentialEndpoints.has(path) && isCurrent(snapshot)) {
      useAuthStore().clearTokens()
    }
    const msg = data && typeof data === 'object' && (data as ApiErrorBody).error
      ? String((data as ApiErrorBody).error)
      : `请求失败 (${res.status})`
    const error = new ApiError(msg, res.status, data)
    reportError(error, { path, status: res.status })
    throw error
  }
  return data as T
}
