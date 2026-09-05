import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, APIError } from './api'

afterEach(() => vi.unstubAllGlobals())
describe('API requests', () => {
  it('sends same-origin credentials and CSRF header for account changes', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"id":"account"}', { status: 201 }))
    vi.stubGlobal('fetch', fetch)
    const result = await api('/accounts', 'POST', { label: '家人' })
    expect(result).toEqual({ id: 'account' })
    expect(fetch).toHaveBeenCalledWith(
      '/api/accounts',
      expect.objectContaining({
        credentials: 'same-origin',
        headers: expect.objectContaining({ 'X-Spoticonn-Request': '1' }),
        body: '{"label":"家人"}',
      }),
    )
  })
  it('preserves authentication errors so the UI can return to login', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response('{"error":"请先登录"}', { status: 401 })),
    )
    await expect(api('/state')).rejects.toMatchObject({ status: 401, message: '请先登录' })
  })
  it('does not display raw HTML from a broken reverse proxy', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response('<html>upstream unavailable</html>', { status: 502 })),
    )
    await expect(api('/state')).rejects.toBeInstanceOf(APIError)
  })
})
