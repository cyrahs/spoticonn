export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message)
  }
}

export async function api<T>(path: string, method = 'GET', data?: unknown): Promise<T> {
  const response = await fetch(`/api${path}`, {
    method,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Spoticonn-Request': '1' },
    body: data === undefined ? undefined : JSON.stringify(data),
  })
  const result = await response.json().catch(() => ({}))
  if (!response.ok)
    throw new APIError(result.error || '暂时无法连接服务，请稍后重试', response.status)
  return result as T
}

export interface Settings {
  name: string
  target_id: string
  volume: number
}
export interface Account {
  id: string
  label: string
  username: string
  device_id: string
  bound: boolean
  status: string
  error?: string
}
export interface Device {
  id: string
  name: string
  address: string
  model: string
  online: boolean
  paired: boolean
  group?: boolean
  waiting_for_leader?: boolean
  members?: { id: string; name: string; model: string; online: boolean }[]
}
export interface Track {
  uri: string
  name: string
  artist_names: string[]
  album_name: string
  album_cover_url: string
  position: number
  duration: number
}
export interface State {
  settings: Settings
  accounts: Account[]
  devices: Device[]
  pairing: { id: string; device_id: string; status: string; error?: string } | null
  playback: {
    account_id: string
    status: string
    output_status: string
    recovering?: boolean
    error?: string
    track: Track | null
    updated_at: string
  }
  diagnostics: { time: string; message: string }[]
  spotify_available: boolean
  airplay_available: boolean
}
