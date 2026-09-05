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
it('creates a pairing endpoint with a label and shows the exact Spotify device name', async () => {
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
        status: 'waiting_spotify',
      })
      return new Response('{}', { status: 201 })
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '添加账号' }))
  await user.type(screen.getByLabelText('账号备注'), '我的账号')
  await user.click(screen.getByRole('button', { name: '创建配对设备' }))
  expect(await screen.findByText('在 Spotify App 中选择「客厅 · 配对 abcd」')).toBeDefined()
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
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
