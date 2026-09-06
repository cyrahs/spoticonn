// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import App from './App'
import type { State } from './api'

const empty: State = {
  settings: { name: '客厅', target_id: '', volume: 30 },
  accounts: [],
  devices: [],
  pairing: null,
  playback: {
    account_id: '',
    status: 'idle',
    output_status: 'disconnected',
    track: null,
    updated_at: new Date().toISOString(),
  },
  diagnostics: [],
  spotify_available: true,
  airplay_available: true,
}
class Events {
  addEventListener() {}
  close() {}
}
beforeEach(() => vi.stubGlobal('EventSource', Events))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})
it('requires a management login before showing accounts', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue(new Response('{"error":"请先登录"}', { status: 401 })),
  )
  render(<App />)
  expect(await screen.findByLabelText('管理密码')).toBeDefined()
  expect(screen.queryByRole('button', { name: '添加账号' })).toBeNull()
})
it('creates an OAuth login and submits the pasted callback without navigating to it', async () => {
  const state: State = structuredClone(empty)
  const fetch = vi.fn().mockImplementation(async (url: string, opts?: RequestInit) => {
    if (url === '/api/accounts' && opts?.method === 'POST') {
      const { label } = JSON.parse(opts.body as string)
      state.accounts.push({
        id: 'abcd1234',
        label,
        username: '',
        device_id: 'stable-id',
        bound: false,
        status: 'waiting_oauth',
        authorization: {
          url: 'https://accounts.spotify.com/authorize?state=test',
          expires_at: new Date(Date.now() + 600000).toISOString(),
        },
      })
      return new Response('{}', { status: 201 })
    }
    if (url === '/api/accounts/abcd1234/oauth') {
      expect(opts?.method).toBe('POST')
      expect(JSON.parse(opts!.body as string)).toEqual({
        callback_url: 'http://127.0.0.1:36842/login?code=test&state=test',
      })
      state.accounts[0].status = 'connecting'
      delete state.accounts[0].authorization
      return new Response('{}')
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '添加账号' }))
  await user.type(screen.getByLabelText('账号备注'), '我的账号')
  await user.click(screen.getByRole('button', { name: '开始登录' }))
  const link = await screen.findByRole('link', { name: '登录 Spotify' })
  expect(link.getAttribute('href')).toBe('https://accounts.spotify.com/authorize?state=test')
  expect(link.getAttribute('target')).toBe('_blank')
  expect(link.getAttribute('rel')).toContain('noreferrer')
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  const callback = screen.getByLabelText('授权后的完整回调地址')
  await user.type(callback, 'http://127.0.0.1:36842/login?code=test&state=test')
  await user.click(screen.getByRole('button', { name: '完成登录' }))
  await waitFor(() => expect(screen.queryByLabelText('授权后的完整回调地址')).toBeNull())
  expect(screen.getByText('正在连接')).toBeDefined()
})

it('clears a submitted callback and shows an authorization error', async () => {
  const state: State = structuredClone(empty)
  state.accounts = [
    {
      id: 'a',
      label: 'A',
      username: '',
      device_id: 'stable',
      bound: false,
      status: 'waiting_oauth',
      authorization: {
        url: 'https://accounts.spotify.com/authorize?state=current',
        expires_at: new Date(Date.now() + 600000).toISOString(),
      },
    },
  ]
  vi.stubGlobal(
    'fetch',
    vi
      .fn()
      .mockImplementation(async (url: string) =>
        url.endsWith('/oauth')
          ? new Response(JSON.stringify({ error: '授权地址与本次登录不匹配' }), { status: 400 })
          : new Response(JSON.stringify(state)),
      ),
  )
  const user = userEvent.setup()
  render(<App />)
  const callback = (await screen.findByLabelText('授权后的完整回调地址')) as HTMLInputElement
  await user.type(callback, 'http://127.0.0.1:36842/login?code=old&state=old')
  await user.click(screen.getByRole('button', { name: '完成登录' }))
  expect(await screen.findByText('授权地址与本次登录不匹配')).toBeDefined()
  expect(callback.value).toBe('')
})

it('offers re-login after expiry and preserves the account identity', async () => {
  const state: State = structuredClone(empty)
  state.accounts = [
    {
      id: 'a',
      label: 'A',
      username: '',
      device_id: 'stable',
      bound: false,
      status: 'expired',
      error: '授权已超时，请重新登录',
    },
  ]
  const fetch = vi.fn().mockImplementation(async (url: string, opts?: RequestInit) => {
    if (url === '/api/accounts/a/rebind') {
      expect(opts?.method).toBe('POST')
      state.accounts[0].status = 'waiting_oauth'
      state.accounts[0].authorization = {
        url: 'https://accounts.spotify.com/authorize?state=new',
        expires_at: new Date(Date.now() + 600000).toISOString(),
      }
      delete state.accounts[0].error
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '重新登录 A' }))
  expect(
    (await screen.findByRole('link', { name: '登录 Spotify' })).getAttribute('href'),
  ).toContain('state=new')
  expect(state.accounts[0].device_id).toBe('stable')
})
it('displays an unavailable audio engine as a real setup problem', async () => {
  vi.stubGlobal(
    'fetch',
    vi
      .fn()
      .mockImplementation(
        async () => new Response(JSON.stringify({ ...empty, airplay_available: false })),
      ),
  )
  render(<App />)
  expect(await screen.findByText(/音频引擎尚未就绪/)).toBeDefined()
  const play = screen.getByRole('button', { name: '继续播放' }) as HTMLButtonElement
  expect(play.disabled).toBe(true)
})

it('lets the user cancel output recovery with the playback control', async () => {
  const state: State = structuredClone(empty)
  state.accounts = [
    {
      id: 'a',
      label: 'A',
      username: 'alice',
      device_id: 'device-a',
      bound: true,
      status: 'online',
    },
  ]
  state.playback = {
    ...state.playback,
    account_id: 'a',
    status: 'paused',
    output_status: 'error',
    recovering: true,
  }
  const fetch = vi.fn().mockImplementation(async (url: string, opts?: RequestInit) => {
    if (url === '/api/playback') {
      expect(JSON.parse(opts!.body as string).action).toBe('pause')
      state.playback.recovering = false
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '取消自动恢复' }))
  expect(await screen.findByRole('button', { name: '继续播放' })).toBeDefined()
})

it('closes a dialog with Escape and returns focus to its trigger', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation(async () => new Response(JSON.stringify(empty))),
  )
  const user = userEvent.setup()
  render(<App />)
  const trigger = await screen.findByRole('button', { name: '添加账号' })
  await user.click(trigger)
  expect(screen.getByRole('dialog')).toBeDefined()
  await user.keyboard('{Escape}')
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(document.activeElement).toBe(trigger)
})

const livingRoom: State['devices'][number] = {
  id: 'tv',
  name: 'Living Room',
  address: '192.0.2.10',
  model: 'AppleTV14,1',
  online: true,
  paired: false,
  group: true,
  members: [
    { id: 'tv', name: 'Living Room', model: 'AppleTV14,1', online: true },
    { id: 'pod', name: 'Living Room (2)', model: 'AudioAccessory6,1', online: true },
  ],
}

it('shows one Apple TV + HomePod output and selects and pairs its connection endpoint', async () => {
  const state: State = structuredClone(empty)
  state.devices = [structuredClone(livingRoom)]
  const fetch = vi.fn().mockImplementation(async (url: string, opts?: RequestInit) => {
    if (url === '/api/settings' && opts?.method === 'PUT') {
      state.settings = JSON.parse(opts.body as string)
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  const target = await screen.findByRole('button', { name: '选择 Living Room' })
  expect(screen.getByText('Apple TV + HomePod')).toBeDefined()
  expect(screen.getByText('通过 Apple TV 连接')).toBeDefined()
  expect(screen.queryByRole('button', { name: '选择 Living Room (2)' })).toBeNull()
  await user.click(target)
  await waitFor(() => expect(target.getAttribute('aria-pressed')).toBe('true'))
  expect(fetch).toHaveBeenCalledWith(
    '/api/settings',
    expect.objectContaining({
      method: 'PUT',
      body: JSON.stringify({ ...empty.settings, target_id: 'tv' }),
    }),
  )
  await user.click(screen.getByRole('button', { name: '输入配对码连接' }))
  await waitFor(() =>
    expect(fetch).toHaveBeenCalledWith(
      '/api/airplay/pairings',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ device_id: 'tv' }),
      }),
    ),
  )
})

it('keeps the saved group selected and disables connection while its leader is unavailable', async () => {
  const state: State = structuredClone(empty)
  state.settings.target_id = 'tv'
  state.devices = [{ ...structuredClone(livingRoom), online: false, waiting_for_leader: true }]
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation(async () => new Response(JSON.stringify(state))),
  )
  render(<App />)
  const target = (await screen.findByRole('button', {
    name: '选择 Living Room',
  })) as HTMLButtonElement
  expect(target.disabled).toBe(true)
  expect(target.getAttribute('aria-pressed')).toBe('true')
  expect(screen.getByText('等待主设备上线')).toBeDefined()
  expect(
    (screen.getByRole('button', { name: '输入配对码连接' }) as HTMLButtonElement).disabled,
  ).toBe(true)
})
