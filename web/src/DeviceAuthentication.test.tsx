// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import App from './App'
import type { State } from './api'

const fixture: State = {
  settings: { name: '客厅', target_id: 'tv', volume: 30 },
  accounts: [],
  devices: [
    {
      id: 'tv',
      name: '客厅',
      address: '192.0.2.10',
      model: 'AppleTV14,1',
      online: true,
      paired: false,
      group: true,
      staged: true,
      members: [
        {
          id: 'tv',
          name: '电视',
          model: 'AppleTV14,1',
          online: true,
          authentication: { requirement: 'pin', password_saved: false },
        },
        {
          id: 'pod',
          name: '音箱',
          model: 'AudioAccessory6,1',
          online: true,
          authentication: { requirement: 'unknown', password_saved: false },
        },
      ],
    },
  ],
  pairing: null,
  playback: {
    account_id: '',
    status: 'idle',
    output_status: 'disconnected',
    track: null,
    updated_at: '',
  },
  diagnostics: [],
  spotify_available: true,
  airplay_available: true,
}
class Events {
  addEventListener() {}
  close() {}
}
let state: State
beforeEach(() => {
  state = structuredClone(fixture)
  vi.stubGlobal('EventSource', Events)
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation(async () => new Response(JSON.stringify(state))),
  )
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

it('keeps unknown authentication optional and targets advanced pairing at the physical HomePod', async () => {
  delete state.devices[0].members![1].authentication
  const user = userEvent.setup()
  render(<App />)
  const pod = await screen.findByRole('region', { name: '音箱 · HomePod 认证' })
  expect(within(pod).getByText(/认证需求未知/)).toBeDefined()
  expect(pod.querySelectorAll(':scope > button')).toHaveLength(0)
  expect(pod.querySelector('details')?.open).toBe(false)
  await user.click(within(pod).getByText('高级认证选项'))
  expect(within(pod).getByText(/已能播放时无需操作/)).toBeDefined()
  await user.click(within(pod).getByRole('button', { name: '尝试屏幕 PIN 配对 音箱 · HomePod' }))
  expect(fetch).toHaveBeenCalledWith(
    '/api/airplay/pairings',
    expect.objectContaining({
      method: 'POST',
      body: JSON.stringify({ device_id: 'tv', member_id: 'pod' }),
    }),
  )
})

it('saves a device password for the HomePod without using the PIN endpoint', async () => {
  state.devices[0].members![1].authentication!.requirement = 'password'
  const fetch = vi.fn().mockImplementation(async (url: string, opts?: RequestInit) => {
    if (url === '/api/airplay/passwords') {
      expect(JSON.parse(opts!.body as string)).toEqual({
        device_id: 'tv',
        member_id: 'pod',
        password: 'a long password! 123',
      })
      state.devices[0].members![1].authentication!.password_saved = true
      return new Response('{}')
    }
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '输入设备密码 音箱 · HomePod' }))
  expect(screen.getByRole('dialog', { name: '设备密码 · 音箱 · HomePod' })).toBeDefined()
  expect(screen.getByText(/保存成功不代表认证已通过/)).toBeDefined()
  const input = screen.getByLabelText('AirPlay 设备密码') as HTMLInputElement
  expect(input.type).toBe('password')
  await user.type(input, 'a long password! 123')
  await user.click(screen.getByRole('button', { name: '保存设备密码' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(screen.getByText(/已保存密码，连接时验证/)).toBeDefined()
  expect(fetch.mock.calls.some(([url]) => url === '/api/airplay/pairings')).toBe(false)
})

it('clears a rejected password and explains a server-side access restriction', async () => {
  state.devices[0].members![1].authentication!.requirement = 'password'
  vi.stubGlobal(
    'fetch',
    vi
      .fn()
      .mockImplementation(async (url: string) =>
        url === '/api/airplay/passwords'
          ? new Response(
              JSON.stringify({ error: '设备限制了家庭访问权限，请先检查 AirPlay 访问设置' }),
              { status: 400 },
            )
          : new Response(JSON.stringify(state)),
      ),
  )
  const user = userEvent.setup()
  render(<App />)
  await user.click(await screen.findByRole('button', { name: '输入设备密码 音箱 · HomePod' }))
  const input = screen.getByLabelText('AirPlay 设备密码') as HTMLInputElement
  await user.type(input, 'bad-password')
  await user.click(screen.getByRole('button', { name: '保存设备密码' }))
  expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveProperty(
    'textContent',
    '设备限制了家庭访问权限，请先检查 AirPlay 访问设置',
  )
  expect(input.value).toBe('')
})

it('shows saved credentials independently and keeps re-pairing in advanced options', async () => {
  state.devices[0].members![0].paired = true
  render(<App />)
  const tv = await screen.findByRole('region', { name: '电视 · Apple TV 认证' })
  expect(within(tv).getByText(/已保存配对凭据/)).toBeDefined()
  expect(tv.querySelectorAll(':scope > button')).toHaveLength(0)
  expect(tv.querySelector('details')?.open).toBe(false)
  const pod = screen.getByRole('region', { name: '音箱 · HomePod 认证' })
  expect(within(pod).queryByText(/已保存配对凭据/)).toBeNull()
})

it('explains home access restrictions without offering an inapplicable authentication flow', async () => {
  state.devices[0].members![1].authentication!.requirement = 'access_control'
  render(<App />)
  const pod = await screen.findByRole('region', { name: '音箱 · HomePod 认证' })
  expect(within(pod).getByText(/访问策略限制/)).toBeDefined()
  expect(within(pod).queryByRole('button')).toBeNull()
  expect(screen.getByRole('button', { name: '配对 电视 · Apple TV' })).toBeDefined()
})

it('identifies the PIN pairing member in the dialog and accepts the TV screen code', async () => {
  state.pairing = { id: 'pair-tv', device_id: 'tv', status: 'waiting_pin' }
  const fetch = vi.fn().mockImplementation(async (url: string) => {
    if (url === '/api/airplay/pairings/pair-tv/pin') state.pairing!.status = 'paired'
    return new Response(JSON.stringify(state))
  })
  vi.stubGlobal('fetch', fetch)
  const user = userEvent.setup()
  render(<App />)
  expect(await screen.findByRole('dialog', { name: '连接 电视 · Apple TV' })).toBeDefined()
  await user.type(screen.getByLabelText('配对码'), '1234')
  await user.click(screen.getByRole('button', { name: '确认配对' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(fetch).toHaveBeenCalledWith(
    '/api/airplay/pairings/pair-tv/pin',
    expect.objectContaining({ method: 'POST', body: JSON.stringify({ pin: '1234' }) }),
  )
})

it('does not require pairing for a standalone HomePod that advertises direct playback', async () => {
  state.devices = [
    {
      id: 'pod',
      name: '音箱',
      model: 'AudioAccessory6,1',
      address: '192.0.2.11',
      online: true,
      paired: false,
      authentication: { requirement: 'none', password_saved: false },
    },
  ]
  render(<App />)
  const pod = await screen.findByRole('region', { name: '音箱 · HomePod 认证' })
  expect(within(pod).getByText(/无需额外配对/)).toBeDefined()
  expect(pod.querySelectorAll(':scope > button')).toHaveLength(0)
})
