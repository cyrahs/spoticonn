import { useEffect, useRef, useState } from 'react'
import type { FormEvent, ReactNode } from 'react'
import {
  Activity,
  Airplay,
  ArrowRight,
  Check,
  ChevronRight,
  CircleHelp,
  Home,
  LoaderCircle,
  LockKeyhole,
  LogOut,
  Music2,
  Pause,
  Play,
  Plus,
  Radio,
  RefreshCw,
  Settings2,
  ShieldCheck,
  SkipBack,
  SkipForward,
  Speaker,
  Trash2,
  TvMinimal,
  Users,
  Volume2,
  Waves,
  X,
} from 'lucide-react'
import { api, APIError } from './api'
import type { Account, Device, State } from './api'

const accountStatus: Record<string, string> = {
  online: '已连接',
  starting: '正在启动',
  connecting: '正在连接',
  waiting_oauth: '等待授权',
  authorizing: '正在登录',
  oauth_error: '需要重新登录',
  expired: '授权超时',
  duplicate: '重复账号',
  error: '需要检查',
}
const outputStatus: Record<string, string> = {
  disconnected: '等待播放',
  connecting: '正在连接',
  connected: '准备播放',
  playing: '正在播放',
  error: '连接异常',
}
const clock = (n: number) =>
  `${Math.floor(n / 60000)}:${String(Math.floor(n / 1000) % 60).padStart(2, '0')}`

const isAppleTV = (model: string) => model.toLowerCase().startsWith('appletv')
const isHomePod = (model: string) => model.toLowerCase().startsWith('audioaccessory')
const isHomeTheater = (d: Device) =>
  d.group && d.members?.some((m) => isAppleTV(m.model)) && d.members.some((m) => isHomePod(m.model))
function deviceType(d: Device) {
  if (isHomeTheater(d)) return 'Apple TV + HomePod'
  if (d.group) return 'AirPlay 音频组'
  if (isAppleTV(d.model)) return 'Apple TV'
  if (isHomePod(d.model)) return 'HomePod'
  return 'AirPlay'
}
function deviceStatus(d: Device) {
  if (d.waiting_for_leader) return '等待主设备上线'
  if (!d.online) return '离线 · 等待重连'
  return d.paired ? '已配对 · 在线' : '在线'
}
function DeviceSymbol({ device: d }: { device: Device }) {
  return (
    <div
      className={`device-symbol ${isHomeTheater(d) ? 'device-symbol-group' : ''}`}
      aria-hidden="true"
    >
      {isAppleTV(d.model) || isHomeTheater(d) ? <TvMinimal size={23} /> : <Speaker size={23} />}
      {isHomeTheater(d) && <Speaker className="group-speaker" size={15} />}
    </div>
  )
}

function Brand() {
  return (
    <div className="brand">
      <span className="brand-icon">
        <Waves size={23} strokeWidth={2.4} />
      </span>
      <span>
        spoticonn<span className="brand-dot">.</span>
      </span>
    </div>
  )
}
function Badge({ children, good = false }: { children: ReactNode; good?: boolean }) {
  return (
    <span className={`badge ${good ? 'good' : ''}`}>
      <i />
      {children}
    </span>
  )
}
function Modal({
  title,
  children,
  close,
}: {
  title: string
  children: ReactNode
  close: () => void
}) {
  const dialog = useRef<HTMLElement>(null)
  const onClose = useRef(close)
  const before = useRef(document.activeElement as HTMLElement | null)
  onClose.current = close
  useEffect(() => {
    const root = dialog.current!
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const focusable = () =>
      Array.from(
        root.querySelectorAll<HTMLElement>(
          'button:not([disabled]), input:not([disabled]), a[href], [tabindex="0"]',
        ),
      )
    if (!root.contains(document.activeElement)) focusable()[0]?.focus()
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose.current()
        return
      }
      if (event.key !== 'Tab') return
      const items = focusable(),
        first = items[0],
        last = items.at(-1)
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last?.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first?.focus()
      }
    }
    root.addEventListener('keydown', keydown)
    return () => {
      root.removeEventListener('keydown', keydown)
      document.body.style.overflow = previousOverflow
      before.current?.focus()
    }
  }, [])
  return (
    <div className="modal-backdrop" onClick={close}>
      <section
        ref={dialog}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="modal"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="section-heading">
          <h2>{title}</h2>
          <button className="icon-button" aria-label="关闭" onClick={close}>
            <X size={20} />
          </button>
        </div>
        {children}
      </section>
    </div>
  )
}

function SpotifyAuthorization({
  account,
  busy,
  error,
  submit,
}: {
  account: Account
  busy: boolean
  error: string
  submit: (callback: string) => void
}) {
  const [callback, setCallback] = useState('')
  return (
    <div className="enrollment-hint">
      <ShieldCheck size={20} />
      <div className="oauth-enrollment">
        <strong>登录「{account.label}」的 Spotify 账号</strong>
        <p>
          在新页面登录要添加的 Premium
          账号并允许授权。完成后，浏览器会跳到一个无法访问的本机地址，这是正常的。
        </p>
        <a
          className="primary compact oauth-link"
          href={account.authorization!.url}
          target="_blank"
          rel="noopener noreferrer"
        >
          登录 Spotify <ArrowRight size={16} />
        </a>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            const value = callback.trim()
            setCallback('')
            submit(value)
          }}
        >
          <label htmlFor="spotify-callback">授权后的完整回调地址</label>
          <input
            id="spotify-callback"
            type="text"
            autoComplete="off"
            spellCheck={false}
            required
            placeholder="http://127.0.0.1:36842/login?code=…&state=…"
            aria-describedby="spotify-callback-help"
            value={callback}
            onChange={(event) => setCallback(event.target.value)}
          />
          <p id="spotify-callback-help">
            复制授权后浏览器地址栏中的完整地址，回到这里粘贴。请保留 code 和 state；链接在{' '}
            {new Date(account.authorization!.expires_at).toLocaleTimeString('zh-CN', {
              hour: '2-digit',
              minute: '2-digit',
            })}{' '}
            前有效。
          </p>
          <button className="primary compact" disabled={busy || !callback.trim()}>
            完成登录
          </button>
          {error && (
            <p className="error-text" role="alert">
              {error}
            </p>
          )}
        </form>
      </div>
    </div>
  )
}

export default function App() {
  const [state, setState] = useState<State | null>(null)
  const [authenticated, setAuthenticated] = useState(false)
  const [loading, setLoading] = useState(true)
  const [connected, setConnected] = useState(true)
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [modal, setModal] = useState<'account' | 'settings' | 'help' | null>(null)
  const [label, setLabel] = useState('')
  const [name, setName] = useState('')
  const [pin, setPin] = useState('')
  const [remove, setRemove] = useState<Account | null>(null)
  const [volume, setVolume] = useState(30)
  const [seek, setSeek] = useState<number | null>(null)

  async function refresh() {
    const v = await api<State>('/state')
    setState(v)
    setAuthenticated(true)
    return v
  }
  useEffect(() => {
    refresh()
      .catch((e) => {
        if (!(e instanceof APIError && e.status === 401)) setError(e.message)
      })
      .finally(() => setLoading(false))
  }, [])
  useEffect(() => {
    if (!authenticated) return
    const events = new EventSource('/api/events')
    events.addEventListener('state', (e) => {
      setState(JSON.parse((e as MessageEvent).data))
      setConnected(true)
    })
    events.onopen = () => setConnected(true)
    events.onerror = () => {
      setConnected(false)
      api('/state').catch((e) => {
        if (e instanceof APIError && e.status === 401) {
          setAuthenticated(false)
          setState(null)
          events.close()
        }
      })
    }
    return () => events.close()
  }, [authenticated])
  useEffect(() => {
    if (state) setVolume(state.settings.volume)
  }, [state?.settings.volume])

  async function action(task: () => Promise<unknown>) {
    setBusy(true)
    setError('')
    try {
      await task()
      await refresh()
    } catch (e) {
      const err = e as Error
      setError(err.message)
      if (e instanceof APIError && e.status === 401) setAuthenticated(false)
    } finally {
      setBusy(false)
    }
  }
  async function login(e: FormEvent) {
    e.preventDefault()
    await action(async () => {
      await api('/auth/login', 'POST', { password })
      setPassword('')
    })
  }
  async function logout() {
    try {
      await api('/auth/logout', 'POST')
      setAuthenticated(false)
      setState(null)
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const playback = (command: string, value = 0) =>
    action(() => api('/playback', 'POST', { action: command, value }))
  const target = state?.devices.find((d) => d.id === state.settings.target_id)
  const current = state?.accounts.find((a) => a.id === state.playback.account_id)
  const track = state?.playback.track
  const playing = state?.playback.status === 'playing'
  const recovering = state?.playback.recovering
  const canPause = playing || recovering || state?.playback.status === 'buffering'
  const pending = state?.accounts.find((a) => !a.bound && a.authorization)
  const pinWaiting = state?.pairing?.status === 'waiting_pin'
  const pairingActive =
    !!state?.pairing && ['starting', 'waiting_pin', 'verifying'].includes(state.pairing.status)

  if (loading)
    return (
      <main className="login-page">
        <Brand />
        <LoaderCircle className="spin" aria-label="加载中" />
      </main>
    )
  if (!authenticated || !state)
    return (
      <main className="login-page">
        <div className="login-wrap">
          <Brand />
          <div className="login-art">
            <div className="orbit orbit-one" />
            <div className="orbit orbit-two" />
            <div className="login-speaker">
              <Speaker size={68} strokeWidth={1} />
            </div>
            <span className="floating-note">
              <Music2 size={22} />
            </span>
          </div>
          <p className="eyebrow">YOUR HOME, CONNECTED</p>
          <h1>音乐常在家。</h1>
          <p className="login-intro">
            连接你的 Spotify 与家中的声音。
            <br />
            设置一次，让音乐随时就位。
          </p>
          <form onSubmit={login}>
            <label htmlFor="password">管理密码</label>
            <div className="input-icon">
              <LockKeyhole size={17} />
              <input
                id="password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="输入管理密码"
              />
            </div>
            {error && (
              <p className="error-text" role="alert">
                {error}
              </p>
            )}
            <button className="primary login-button" disabled={busy}>
              {busy ? (
                <LoaderCircle size={18} className="spin" />
              ) : (
                <>
                  进入控制台
                  <ArrowRight size={18} />
                </>
              )}
            </button>
          </form>
          <p className="privacy-note">
            <ShieldCheck size={14} /> 配置和账号凭据保存在你的 home 设备
          </p>
        </div>
        <span className="login-footer">SPOTIFY CONNECT → AIRPLAY</span>
      </main>
    )

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <Brand />
        <div className="sidebar-label">家庭音频</div>
        <nav>
          <a className="nav-item active" href="#overview">
            <Home size={18} />
            总览
            <span className="nav-active" />
          </a>
          <a className="nav-item" href="#accounts">
            <Users size={18} />
            Spotify 账号<span className="nav-count">{state.accounts.length}</span>
          </a>
          <a className="nav-item" href="#outputs">
            <Airplay size={18} />
            AirPlay 输出
          </a>
          <button
            className="nav-item"
            onClick={() => {
              setName(state.settings.name)
              setModal('settings')
            }}
          >
            <Settings2 size={18} />
            设置
          </button>
        </nav>
        <div className="sidebar-bottom">
          <div className="local-card">
            <span className="local-icon">
              <ShieldCheck size={20} />
            </span>
            <strong>运行在你的家中</strong>
            <p>网页关闭后，音乐依然继续。</p>
            <span>
              <i className="dot" /> 内网 / VPN 管理
            </span>
          </div>
          <button className="help-button" onClick={() => setModal('help')}>
            <CircleHelp size={16} />
            连接指南
            <ChevronRight size={15} />
          </button>
        </div>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <span className="breadcrumb">
            家庭音频
            <ChevronRight size={13} />
            <strong>总览</strong>
          </span>
          <div className="topbar-right">
            <Badge good={connected}>{connected ? '服务已连接' : '正在重连'}</Badge>
            <button className="icon-button" aria-label="退出登录" onClick={() => void logout()}>
              <LogOut size={17} />
            </button>
          </div>
        </header>
        <main className="content" id="overview">
          <div className="page-heading">
            <div>
              <p className="eyebrow">A LITTLE CLOSER TO YOUR MUSIC</p>
              <h1>把音乐，留在家里。</h1>
              <p>从任意已绑定的 Spotify 账号，播放到你喜欢的声音。</p>
            </div>
            <button
              className="secondary settings-shortcut"
              onClick={() => {
                setName(state.settings.name)
                setModal('settings')
              }}
            >
              <Settings2 size={16} />
              设备设置
            </button>
          </div>
          {error && !pending && (
            <div className="alert" role="alert">
              <span>{error}</span>
              <button className="icon-button" aria-label="关闭提示" onClick={() => setError('')}>
                <X size={16} />
              </button>
            </div>
          )}
          {(!state.spotify_available || !state.airplay_available) && (
            <div className="alert warning">
              <Activity size={18} />
              <span>
                音频引擎尚未就绪：{!state.spotify_available && 'go-librespot '}
                {!state.airplay_available && 'cliairplay '}
                。请按部署指南安装引擎；管理界面可以正常使用。
              </span>
            </div>
          )}
          <section className="route-card" aria-label="音频链路">
            <div className="route-top">
              <span className="micro-label">你的音频链路</span>
              <Badge good={playing}>
                {outputStatus[state.playback.output_status] || '等待连接'}
              </Badge>
            </div>
            <div className="route-flow">
              <div className="route-node">
                <div className="route-icon spotify-color">
                  <Radio size={26} />
                </div>
                <strong>Spotify</strong>
                <small>{state.accounts.filter((a) => a.bound).length} 个已绑定账号</small>
              </div>
              <div className={`connector ${playing ? 'live' : ''}`}>
                <span />
                <ArrowRight size={14} />
                <span />
              </div>
              <div className="route-node">
                <div className="route-icon home-color">
                  <Waves size={28} />
                </div>
                <strong>{state.settings.name}</strong>
                <small>Connect 常驻接收器</small>
              </div>
              <div className={`connector ${playing ? 'live' : ''}`}>
                <span />
                <ArrowRight size={14} />
                <span />
              </div>
              <div className="route-node">
                <div className="route-icon">
                  <TvMinimal size={27} />
                </div>
                <strong>{target?.name || '选择 AirPlay 输出'}</strong>
                <small>{target ? deviceType(target) : '让家中的设备加入链路'}</small>
              </div>
              <div className="route-end">
                <span>♪</span>
                <span>♫</span>
              </div>
            </div>
          </section>
          <div className="dashboard-grid">
            <section className="card player-card">
              <div className="section-heading">
                <h2>
                  <Music2 size={17} />
                  正在播放
                </h2>
                <span className="subtle">{current?.label || '准备就绪'}</span>
              </div>
              <div className={`record-art ${playing ? 'is-playing' : ''}`}>
                <div className="record-ring ring-1" />
                <div className="record-ring ring-2" />
                <div className="record-ring ring-3" />
                {track?.album_cover_url ? (
                  <img
                    className="cover"
                    src={track.album_cover_url}
                    alt={`${track.album_name} 专辑封面`}
                  />
                ) : (
                  <div className="vinyl">
                    <div>
                      <Waves size={29} />
                    </div>
                  </div>
                )}
                <span className="art-caption">GOOD MUSIC. RIGHT AT HOME.</span>
                <div className="art-equalizer">
                  <i />
                  <i />
                  <i />
                  <i />
                  <i />
                </div>
              </div>
              <div className="track-info">
                <h3>{track?.name || '下一首，由你决定。'}</h3>
                <p>
                  {track
                    ? track.artist_names.join(' · ')
                    : `在 Spotify 中选择「${state.settings.name}」开始播放`}
                </p>
              </div>
              <div className="progress-area">
                <input
                  aria-label="播放进度"
                  type="range"
                  min={0}
                  max={track?.duration || 1}
                  value={seek ?? track?.position ?? 0}
                  disabled={!track || busy}
                  onChange={(e) => setSeek(Number(e.target.value))}
                  onPointerUp={(e) => {
                    void playback('seek', Number(e.currentTarget.value))
                    setSeek(null)
                  }}
                  onKeyUp={(e) => {
                    if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) {
                      void playback('seek', Number(e.currentTarget.value))
                      setSeek(null)
                    }
                  }}
                />
                <div>
                  <span>{clock(seek ?? track?.position ?? 0)}</span>
                  <span>{clock(track?.duration ?? 0)}</span>
                </div>
              </div>
              <div className="transport">
                <button
                  aria-label="上一首"
                  disabled={!current || busy}
                  onClick={() => void playback('prev')}
                >
                  <SkipBack size={21} fill="currentColor" />
                </button>
                <button
                  className="play-button"
                  aria-label={recovering ? '取消自动恢复' : canPause ? '暂停' : '继续播放'}
                  disabled={!current || busy}
                  onClick={() => void playback(canPause ? 'pause' : 'resume')}
                >
                  {state.playback.status === 'buffering' ? (
                    <LoaderCircle className="spin" size={23} />
                  ) : canPause ? (
                    <Pause size={22} fill="currentColor" />
                  ) : (
                    <Play size={23} fill="currentColor" />
                  )}
                </button>
                <button
                  aria-label="下一首"
                  disabled={!current || busy}
                  onClick={() => void playback('next')}
                >
                  <SkipForward size={21} fill="currentColor" />
                </button>
              </div>
              <div className="volume-control">
                <Volume2 size={17} />
                <input
                  aria-label="输出音量"
                  type="range"
                  min={0}
                  max={100}
                  value={volume}
                  onChange={(e) => setVolume(Number(e.target.value))}
                  onPointerUp={(e) => void playback('volume', Number(e.currentTarget.value))}
                  onKeyUp={(e) => {
                    if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key))
                      void playback('volume', Number(e.currentTarget.value))
                  }}
                  disabled={busy}
                />
                <span>{volume}%</span>
              </div>
              {recovering && <p className="inline-error">正在自动恢复连接，点击暂停可取消。</p>}
              {state.playback.error && (
                <p role="status" className="inline-error">
                  {state.playback.error}
                </p>
              )}
            </section>
            <section className="card output-card" id="outputs">
              <div className="section-heading">
                <h2>
                  <Airplay size={17} />
                  AirPlay 输出
                </h2>
                <span className="discovering">
                  <i />
                  自动发现中
                </span>
              </div>
              <p className="card-intro">选择一个目标，所有账号共享这一路声音。</p>
              <div className="device-list">
                {state.devices.length === 0 ? (
                  <div className="empty-devices">
                    <div className="scan-icon">
                      <Airplay size={31} />
                    </div>
                    <strong>正在寻找家中的声音</strong>
                    <p>让 Apple TV 与 home 设备处于同一局域网，并开启 AirPlay。</p>
                    <button className="text-button" onClick={() => setModal('help')}>
                      查看连接指南
                      <ArrowRight size={14} />
                    </button>
                  </div>
                ) : (
                  state.devices.map((d) => (
                    <div
                      className={`device-row ${d.id === state.settings.target_id ? 'selected' : ''}`}
                      key={d.id}
                    >
                      <button
                        className="device-select"
                        aria-label={`选择 ${d.name}`}
                        aria-pressed={d.id === state.settings.target_id}
                        disabled={busy || !d.online}
                        onClick={() =>
                          void action(() =>
                            api('/settings', 'PUT', { ...state.settings, target_id: d.id }),
                          )
                        }
                      >
                        <DeviceSymbol device={d} />
                        <div>
                          <strong>{d.name}</strong>
                          <span>{deviceType(d)}</span>
                          <span>{deviceStatus(d)}</span>
                        </div>
                        <span className="radio-check">
                          {d.id === state.settings.target_id && <Check size={12} />}
                        </span>
                      </button>
                      {d.group && (
                        <p className="device-route">
                          {d.waiting_for_leader
                            ? '发现音频组成员，等待确认连接入口'
                            : isAppleTV(d.model)
                              ? '通过 Apple TV 连接'
                              : isHomePod(d.model)
                                ? '通过 HomePod 连接'
                                : '通过组内主设备连接'}
                        </p>
                      )}
                      <button
                        className="pair-button"
                        disabled={busy || !d.online || pairingActive}
                        onClick={() =>
                          void action(() => api('/airplay/pairings', 'POST', { device_id: d.id }))
                        }
                      >
                        {d.paired ? '重新配对' : '输入配对码连接'}
                        <ChevronRight size={13} />
                      </button>
                    </div>
                  ))
                )}
              </div>
              <div className="output-note">
                <TvMinimal size={16} />
                <p>
                  Apple TV 与 HomePod 属于同一音频组时会合并显示。
                  <br />
                  选择合并后的目标，即可通过组内主设备连接。
                </p>
              </div>
            </section>
          </div>
          <section className="card accounts-card" id="accounts">
            <div className="section-heading">
              <div>
                <h2>
                  <Users size={18} />
                  Spotify 账号<span className="count-pill">{state.accounts.length}</span>
                </h2>
                <p className="card-intro">每个账号独立在线。新播放自动接管，音乐不会混在一起。</p>
              </div>
              <button
                className="primary compact"
                disabled={busy}
                onClick={() => {
                  setLabel('')
                  setModal('account')
                }}
              >
                <Plus size={16} />
                添加账号
              </button>
            </div>
            {state.accounts.length === 0 ? (
              <div className="accounts-empty">
                <div className="avatar empty-avatar">
                  <Users size={23} />
                </div>
                <div>
                  <strong>先把你的 Spotify 带回家</strong>
                  <p>添加一个 Premium 账号，通过 Spotify 网页授权即可登录。</p>
                </div>
              </div>
            ) : (
              <div className="account-list">
                {state.accounts.map((a, index) => (
                  <div className="account-row" key={a.id}>
                    <div className={`avatar avatar-${index % 3}`}>{a.label[0]?.toUpperCase()}</div>
                    <div className="account-name">
                      <strong>
                        {a.label}
                        {current?.id === a.id && playing && (
                          <span className="playing-label">
                            <Waves size={12} />
                            正在播放
                          </span>
                        )}
                      </strong>
                      <span>{a.username || '授权完成后自动保存登录状态'}</span>
                      {a.error && <small className="error-text">{a.error}</small>}
                    </div>
                    <Badge good={a.status === 'online'}>
                      {accountStatus[a.status] || a.status}
                    </Badge>
                    <button
                      className="icon-button"
                      title="重新登录"
                      aria-label={`重新登录 ${a.label}`}
                      disabled={busy}
                      onClick={() => void action(() => api(`/accounts/${a.id}/rebind`, 'POST'))}
                    >
                      <RefreshCw size={15} />
                    </button>
                    <button
                      className="icon-button"
                      title="删除账号"
                      aria-label={`删除 ${a.label}`}
                      disabled={busy}
                      onClick={() => setRemove(a)}
                    >
                      <Trash2 size={15} />
                    </button>
                  </div>
                ))}
              </div>
            )}
            {pending && (
              <SpotifyAuthorization
                key={pending.authorization!.url}
                account={pending}
                busy={busy}
                error={error}
                submit={(callback) =>
                  void action(() =>
                    api(`/accounts/${pending.id}/oauth`, 'POST', { callback_url: callback }),
                  )
                }
              />
            )}
          </section>
          <section className="diagnostics">
            <details>
              <summary>
                <Activity size={15} />
                运行记录
                <span>
                  {state.diagnostics.length ? '最近的连接与状态变化' : '一切就绪，等待你的第一首歌'}
                </span>
              </summary>
              <div className="log-list">
                {state.diagnostics.length ? (
                  [...state.diagnostics].reverse().map((d, i) => (
                    <div key={i}>
                      <time>{new Date(d.time).toLocaleTimeString('zh-CN')}</time>
                      <span>{d.message}</span>
                    </div>
                  ))
                ) : (
                  <p>暂无运行记录。</p>
                )}
              </div>
            </details>
          </section>
          <footer className="footer">
            <span>
              <span className="dot" />
              SPOTICONN · AT HOME
            </span>
            <span>你的账号。你的设备。你的音乐。</span>
          </footer>
        </main>
      </div>
      {modal === 'account' && (
        <Modal title="添加 Spotify 账号" close={() => setModal(null)}>
          <p className="modal-intro">
            为这个账号起一个备注，然后通过 Spotify 网页授权登录。手机无需连接家庭 Wi-Fi。
          </p>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              void action(async () => {
                await api('/accounts', 'POST', { label })
                setModal(null)
              })
            }}
          >
            <label htmlFor="account-label">账号备注</label>
            <input
              id="account-label"
              autoFocus
              maxLength={40}
              required
              placeholder="例如：我的 Spotify"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
            />
            <div className="modal-tip">
              <ShieldCheck size={17} />
              Spotify 密码只在 Spotify 授权页输入。登录完成后，服务会保存此账号的设备凭据。
            </div>
            <button className="primary full" disabled={busy}>
              开始登录
              <ArrowRight size={16} />
            </button>
          </form>
          {error && (
            <p role="alert" className="error-text">
              {error}
            </p>
          )}
        </Modal>
      )}
      {modal === 'settings' && (
        <Modal title="设备设置" close={() => setModal(null)}>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              void action(async () => {
                await api('/settings', 'PUT', { ...state.settings, name })
                setModal(null)
              })
            }}
          >
            <label htmlFor="device-name">Spotify 中显示的播放器名称</label>
            <input
              id="device-name"
              value={name}
              autoFocus
              required
              maxLength={60}
              onChange={(e) => setName(e.target.value)}
            />
            <p className="modal-intro">
              所有已绑定账号看到相同名称。修改名称会重新连接账号，并暂停当前播放。
            </p>
            <button className="primary full" disabled={busy}>
              保存设置
              <Check size={16} />
            </button>
          </form>
          {error && (
            <p role="alert" className="error-text">
              {error}
            </p>
          )}
        </Modal>
      )}
      {modal === 'help' && (
        <Modal title="让音乐连接起来" close={() => setModal(null)}>
          <ol className="guide">
            <li>
              <strong>选择家中的 AirPlay 设备</strong>
              <p>
                让 home 与 Apple TV 位于同一局域网，将 HomePod 设为 Apple TV
                的默认音频输出。发现同组设备后会自动合并，选择标有「Apple TV + HomePod」的目标即可。
              </p>
            </li>
            <li>
              <strong>按需输入配对码</strong>
              <p>首次配对可能需要打开电视查看四位码。完成配对后，日常播放可再验证关电视状态。</p>
            </li>
            <li>
              <strong>添加每个 Premium 账号</strong>
              <p>
                添加账号后打开 Spotify
                授权页，再把授权后的完整回调地址粘贴回来。每次添加一个账号，无需同网配对。
              </p>
            </li>
            <li>
              <strong>随时从 Spotify 开始播放</strong>
              <p>绑定后，各账号都有自己的常驻会话。新账号播放会暂停旧账号，网页不需要保持打开。</p>
            </li>
          </ol>
          <p className="modal-tip">
            找不到设备时，请检查主机网络、局域网接口和 mDNS；这些设置由 home 部署端处理。
          </p>
        </Modal>
      )}
      {pairingActive && state.pairing && (
        <Modal
          title="连接 AirPlay 设备"
          close={() =>
            void action(async () => {
              await api(`/airplay/pairings/${state.pairing!.id}/cancel`, 'POST')
              setPin('')
            })
          }
        >
          <p className="modal-intro">
            {pinWaiting
              ? '请输入 Apple TV 屏幕上显示的四位配对码。'
              : state.pairing.status === 'verifying'
                ? '正在验证配对码…'
                : '正在连接设备，请等待配对提示…'}
          </p>
          {!pinWaiting && <LoaderCircle className="spin" aria-label="正在配对" />}
          {pinWaiting && (
            <form
              onSubmit={(e) => {
                e.preventDefault()
                void action(async () => {
                  await api(`/airplay/pairings/${state.pairing!.id}/pin`, 'POST', { pin })
                  setPin('')
                })
              }}
            >
              <label htmlFor="pair-pin">配对码</label>
              <input
                id="pair-pin"
                className="pin-input"
                inputMode="numeric"
                autoComplete="one-time-code"
                autoFocus
                pattern="[0-9]{4}"
                maxLength={4}
                required
                placeholder="0000"
                value={pin}
                onChange={(e) => setPin(e.target.value.replace(/\D/g, ''))}
              />
              <button className="primary full" disabled={busy}>
                确认配对
                <Check size={16} />
              </button>
            </form>
          )}
          {error && (
            <p role="alert" className="error-text">
              {error}
            </p>
          )}
        </Modal>
      )}
      {state.pairing?.error && (
        <div className="pairing-toast" role="status">
          {state.pairing.error}
        </div>
      )}
      {remove && (
        <Modal title="删除账号" close={() => setRemove(null)}>
          <p className="modal-intro">
            从此设备删除「{remove.label}」的登录凭据？再次使用需要重新授权登录。
          </p>
          <button
            className="danger full"
            disabled={busy}
            onClick={() =>
              void action(async () => {
                await api(`/accounts/${remove.id}`, 'DELETE')
                setRemove(null)
              })
            }
          >
            删除账号
          </button>
        </Modal>
      )}
    </div>
  )
}
